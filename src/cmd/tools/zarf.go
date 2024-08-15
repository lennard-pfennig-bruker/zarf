// SPDX-License-Identifier: Apache-2.0
// SPDX-FileCopyrightText: 2021-Present The Zarf Authors

// Package tools contains the CLI commands for Zarf.
package tools

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"

	"github.com/AlecAivazis/survey/v2"
	"github.com/sigstore/cosign/v2/pkg/cosign"
	"github.com/spf13/cobra"

	"github.com/defenseunicorns/pkg/helpers/v2"
	"github.com/defenseunicorns/pkg/oci"

	"github.com/zarf-dev/zarf/src/cmd/common"
	"github.com/zarf-dev/zarf/src/config"
	"github.com/zarf-dev/zarf/src/config/lang"
	"github.com/zarf-dev/zarf/src/internal/gitea"
	"github.com/zarf-dev/zarf/src/internal/packager/helm"
	"github.com/zarf-dev/zarf/src/internal/packager/template"
	"github.com/zarf-dev/zarf/src/pkg/cluster"
	"github.com/zarf-dev/zarf/src/pkg/logging"
	"github.com/zarf-dev/zarf/src/pkg/message"
	"github.com/zarf-dev/zarf/src/pkg/packager/sources"
	"github.com/zarf-dev/zarf/src/pkg/pki"
	"github.com/zarf-dev/zarf/src/pkg/zoci"
	"github.com/zarf-dev/zarf/src/types"
)

var subAltNames []string
var outputDirectory string
var updateCredsInitOpts types.ZarfInitOptions

var deprecatedGetGitCredsCmd = &cobra.Command{
	Use:    "get-git-password",
	Hidden: true,
	Short:  lang.CmdToolsGetGitPasswdShort,
	Long:   lang.CmdToolsGetGitPasswdLong,
	Run: func(cmd *cobra.Command, _ []string) {
		logging.FromContextOrDiscard(cmd.Context()).Warn("Deprecated: This command has been replaced by 'zarf tools get-creds git' and will be removed in Zarf v1.0.0.")
		getCredsCmd.Run(getCredsCmd, []string{"git"})
	},
}

var getCredsCmd = &cobra.Command{
	Use:     "get-creds",
	Short:   lang.CmdToolsGetCredsShort,
	Long:    lang.CmdToolsGetCredsLong,
	Example: lang.CmdToolsGetCredsExample,
	Aliases: []string{"gc"},
	Args:    cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()

		timeoutCtx, cancel := context.WithTimeout(ctx, cluster.DefaultTimeout)
		defer cancel()
		c, err := cluster.NewClusterWithWait(timeoutCtx)
		if err != nil {
			return err
		}

		state, err := c.LoadZarfState(ctx)
		if err != nil {
			return err
		}
		// TODO: Determine if this is actually needed.
		if state.Distro == "" {
			return errors.New("Zarf state secret did not load properly")
		}

		if len(args) > 0 {
			// If a component name is provided, only show that component's credentials
			message.PrintComponentCredential(state, args[0])
		} else {
			message.PrintCredentialTable(state, nil)
		}
		return nil
	},
}

var updateCredsCmd = &cobra.Command{
	Use:     "update-creds",
	Short:   lang.CmdToolsUpdateCredsShort,
	Long:    lang.CmdToolsUpdateCredsLong,
	Example: lang.CmdToolsUpdateCredsExample,
	Aliases: []string{"uc"},
	Args:    cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		validKeys := []string{message.RegistryKey, message.GitKey, message.ArtifactKey, message.AgentKey}
		if len(args) == 0 {
			args = validKeys
		} else {
			if !slices.Contains(validKeys, args[0]) {
				cmd.Help()
				return fmt.Errorf("invalid service key specified, valid keys are: %s, %s, and %s", message.RegistryKey, message.GitKey, message.ArtifactKey)
			}
		}

		ctx := cmd.Context()
		log := logging.FromContextOrDiscard(ctx)

		timeoutCtx, cancel := context.WithTimeout(ctx, cluster.DefaultTimeout)
		defer cancel()
		c, err := cluster.NewClusterWithWait(timeoutCtx)
		if err != nil {
			return err
		}

		oldState, err := c.LoadZarfState(ctx)
		if err != nil {
			return err
		}
		// TODO: Determine if this is actually needed.
		if oldState.Distro == "" {
			return errors.New("Zarf state secret did not load properly")
		}
		newState, err := cluster.MergeZarfState(oldState, updateCredsInitOpts, args)
		if err != nil {
			return fmt.Errorf("unable to update Zarf credentials: %w", err)
		}

		message.PrintCredentialUpdates(oldState, newState, args)

		confirm := config.CommonOptions.Confirm

		if confirm {
			message.Note(lang.CmdToolsUpdateCredsConfirmProvided)
		} else {
			prompt := &survey.Confirm{
				Message: lang.CmdToolsUpdateCredsConfirmContinue,
			}
			if err := survey.AskOne(prompt, &confirm); err != nil {
				return fmt.Errorf("confirm selection canceled: %w", err)
			}
		}

		if confirm {
			// Update registry and git pull secrets
			if slices.Contains(args, message.RegistryKey) {
				c.UpdateZarfManagedImageSecrets(ctx, newState)
			}
			if slices.Contains(args, message.GitKey) {
				c.UpdateZarfManagedGitSecrets(ctx, newState)
			}

			// Update artifact token (if internal)
			if slices.Contains(args, message.ArtifactKey) && newState.ArtifactServer.PushToken == "" && newState.ArtifactServer.IsInternal() {
				tunnel, err := c.NewTunnel(cluster.ZarfNamespaceName, cluster.SvcResource, cluster.ZarfGitServerName, "", 0, cluster.ZarfGitServerPort)
				if err != nil {
					return err
				}
				_, err = tunnel.Connect(cmd.Context())
				if err != nil {
					return err
				}
				defer tunnel.Close()
				tunnelURL := tunnel.HTTPEndpoint()
				giteaClient, err := gitea.NewClient(tunnelURL, oldState.GitServer.PushUsername, oldState.GitServer.PushPassword)
				if err != nil {
					return err
				}
				err = tunnel.Wrap(func() error {
					tokenSha1, err := giteaClient.CreatePackageRegistryToken(ctx)
					if err != nil {
						return err
					}
					newState.ArtifactServer.PushToken = tokenSha1
					return nil
				})
				if err != nil {
					// Warn if we couldn't actually update the git server (it might not be installed and we should try to continue)
					log.Warn("Unable to create the new Gitea artifact token", "error", err)
				}
			}

			// Save the final Zarf State
			err = c.SaveZarfState(ctx, newState)
			if err != nil {
				return fmt.Errorf("failed to save the Zarf State to the cluster: %w", err)
			}

			// Update Zarf 'init' component Helm releases if present
			h := helm.NewClusterOnly(&types.PackagerConfig{}, template.GetZarfVariableConfig(), newState, c)

			if slices.Contains(args, message.RegistryKey) && newState.RegistryInfo.IsInternal() {
				err = h.UpdateZarfRegistryValues(ctx)
				if err != nil {
					// Warn if we couldn't actually update the registry (it might not be installed and we should try to continue)
					log.Warn("Unable to update Zarf Registry values", "error", err)
				}
			}
			if slices.Contains(args, message.GitKey) && newState.GitServer.IsInternal() {
				tunnel, err := c.NewTunnel(cluster.ZarfNamespaceName, cluster.SvcResource, cluster.ZarfGitServerName, "", 0, cluster.ZarfGitServerPort)
				if err != nil {
					return err
				}
				_, err = tunnel.Connect(cmd.Context())
				if err != nil {
					return err
				}
				defer tunnel.Close()
				tunnelURL := tunnel.HTTPEndpoint()
				giteaClient, err := gitea.NewClient(tunnelURL, oldState.GitServer.PushUsername, oldState.GitServer.PushPassword)
				if err != nil {
					return err
				}
				err = tunnel.Wrap(func() error {
					err := giteaClient.UpdateGitUser(ctx, newState.GitServer.PullUsername, newState.GitServer.PullPassword)
					if err != nil {
						return err
					}
					err = giteaClient.UpdateGitUser(ctx, newState.GitServer.PushUsername, newState.GitServer.PushPassword)
					if err != nil {
						return err
					}
					return nil
				})
				if err != nil {
					// Warn if we couldn't actually update the git server (it might not be installed and we should try to continue)
					log.Warn("Unable to update Zarf Git Server values", "error", err)
				}
			}
			if slices.Contains(args, message.AgentKey) {
				err = h.UpdateZarfAgentValues(ctx)
				if err != nil {
					// Warn if we couldn't actually update the agent (it might not be installed and we should try to continue)
					log.Warn("Unable to update Zarf Agent TLS secrets", "error", err)
				}
			}
		}
		return nil
	},
}

var clearCacheCmd = &cobra.Command{
	Use:     "clear-cache",
	Aliases: []string{"c"},
	Short:   lang.CmdToolsClearCacheShort,
	RunE: func(_ *cobra.Command, _ []string) error {
		message.Notef(lang.CmdToolsClearCacheDir, config.GetAbsCachePath())
		if err := os.RemoveAll(config.GetAbsCachePath()); err != nil {
			return fmt.Errorf("unable to clear the cache directory %s: %w", config.GetAbsCachePath(), err)
		}
		message.Successf(lang.CmdToolsClearCacheSuccess, config.GetAbsCachePath())
		return nil
	},
}

var downloadInitCmd = &cobra.Command{
	Use:   "download-init",
	Short: lang.CmdToolsDownloadInitShort,
	RunE: func(cmd *cobra.Command, _ []string) error {
		url := zoci.GetInitPackageURL(config.CLIVersion)
		remote, err := zoci.NewRemote(url, oci.PlatformForArch(config.GetArch()))
		if err != nil {
			return fmt.Errorf("unable to download the init package: %w", err)
		}
		source := &sources.OCISource{Remote: remote}
		_, err = source.Collect(cmd.Context(), outputDirectory)
		if err != nil {
			return fmt.Errorf("unable to download the init package: %w", err)
		}
		return nil
	},
}

var generatePKICmd = &cobra.Command{
	Use:     "gen-pki HOST",
	Aliases: []string{"pki"},
	Short:   lang.CmdToolsGenPkiShort,
	Args:    cobra.ExactArgs(1),
	RunE: func(_ *cobra.Command, args []string) error {
		pki, err := pki.GeneratePKI(args[0], subAltNames...)
		if err != nil {
			return err
		}
		if err := os.WriteFile("tls.ca", pki.CA, helpers.ReadAllWriteUser); err != nil {
			return err
		}
		if err := os.WriteFile("tls.crt", pki.Cert, helpers.ReadAllWriteUser); err != nil {
			return err
		}
		if err := os.WriteFile("tls.key", pki.Key, helpers.ReadWriteUser); err != nil {
			return err
		}
		message.Successf(lang.CmdToolsGenPkiSuccess, args[0])
		return nil
	},
}

var generateKeyCmd = &cobra.Command{
	Use:     "gen-key",
	Aliases: []string{"key"},
	Short:   lang.CmdToolsGenKeyShort,
	RunE: func(_ *cobra.Command, _ []string) error {
		// Utility function to prompt the user for the password to the private key
		passwordFunc := func(bool) ([]byte, error) {
			// perform the first prompt
			var password string
			prompt := &survey.Password{
				Message: lang.CmdToolsGenKeyPrompt,
			}
			if err := survey.AskOne(prompt, &password); err != nil {
				return nil, fmt.Errorf(lang.CmdToolsGenKeyErrUnableGetPassword, err.Error())
			}

			// perform the second prompt
			var doubleCheck string
			rePrompt := &survey.Password{
				Message: lang.CmdToolsGenKeyPromptAgain,
			}
			if err := survey.AskOne(rePrompt, &doubleCheck); err != nil {
				return nil, fmt.Errorf(lang.CmdToolsGenKeyErrUnableGetPassword, err.Error())
			}

			// check if the passwords match
			if password != doubleCheck {
				return nil, fmt.Errorf(lang.CmdToolsGenKeyErrPasswordsNotMatch)
			}

			return []byte(password), nil
		}

		// Use cosign to generate the keypair
		keyBytes, err := cosign.GenerateKeyPair(passwordFunc)
		if err != nil {
			return fmt.Errorf("unable to generate key pair: %w", err)
		}

		prvKeyFileName := "cosign.key"
		pubKeyFileName := "cosign.pub"

		// Check if we are about to overwrite existing key files
		_, prvKeyExistsErr := os.Stat(prvKeyFileName)
		_, pubKeyExistsErr := os.Stat(pubKeyFileName)
		if prvKeyExistsErr == nil || pubKeyExistsErr == nil {
			var confirm bool
			confirmOverwritePrompt := &survey.Confirm{
				Message: fmt.Sprintf(lang.CmdToolsGenKeyPromptExists, prvKeyFileName),
			}
			err := survey.AskOne(confirmOverwritePrompt, &confirm)
			if err != nil {
				return err
			}
			if !confirm {
				return errors.New("did not receive confirmation for overwriting key file(s)")
			}
		}

		// Write the key file contents to disk
		if err := os.WriteFile(prvKeyFileName, keyBytes.PrivateBytes, helpers.ReadWriteUser); err != nil {
			return err
		}
		if err := os.WriteFile(pubKeyFileName, keyBytes.PublicBytes, helpers.ReadAllWriteUser); err != nil {
			return err
		}

		message.Successf(lang.CmdToolsGenKeySuccess, prvKeyFileName, pubKeyFileName)
		return nil
	},
}

func init() {
	v := common.InitViper()

	toolsCmd.AddCommand(deprecatedGetGitCredsCmd)
	toolsCmd.AddCommand(getCredsCmd)

	toolsCmd.AddCommand(updateCredsCmd)

	// Always require confirm flag (no viper)
	updateCredsCmd.Flags().BoolVar(&config.CommonOptions.Confirm, "confirm", false, lang.CmdToolsUpdateCredsConfirmFlag)

	// Flags for using an external Git server
	updateCredsCmd.Flags().StringVar(&updateCredsInitOpts.GitServer.Address, "git-url", v.GetString(common.VInitGitURL), lang.CmdInitFlagGitURL)
	updateCredsCmd.Flags().StringVar(&updateCredsInitOpts.GitServer.PushUsername, "git-push-username", v.GetString(common.VInitGitPushUser), lang.CmdInitFlagGitPushUser)
	updateCredsCmd.Flags().StringVar(&updateCredsInitOpts.GitServer.PushPassword, "git-push-password", v.GetString(common.VInitGitPushPass), lang.CmdInitFlagGitPushPass)
	updateCredsCmd.Flags().StringVar(&updateCredsInitOpts.GitServer.PullUsername, "git-pull-username", v.GetString(common.VInitGitPullUser), lang.CmdInitFlagGitPullUser)
	updateCredsCmd.Flags().StringVar(&updateCredsInitOpts.GitServer.PullPassword, "git-pull-password", v.GetString(common.VInitGitPullPass), lang.CmdInitFlagGitPullPass)

	// Flags for using an external registry
	updateCredsCmd.Flags().StringVar(&updateCredsInitOpts.RegistryInfo.Address, "registry-url", v.GetString(common.VInitRegistryURL), lang.CmdInitFlagRegURL)
	updateCredsCmd.Flags().StringVar(&updateCredsInitOpts.RegistryInfo.PushUsername, "registry-push-username", v.GetString(common.VInitRegistryPushUser), lang.CmdInitFlagRegPushUser)
	updateCredsCmd.Flags().StringVar(&updateCredsInitOpts.RegistryInfo.PushPassword, "registry-push-password", v.GetString(common.VInitRegistryPushPass), lang.CmdInitFlagRegPushPass)
	updateCredsCmd.Flags().StringVar(&updateCredsInitOpts.RegistryInfo.PullUsername, "registry-pull-username", v.GetString(common.VInitRegistryPullUser), lang.CmdInitFlagRegPullUser)
	updateCredsCmd.Flags().StringVar(&updateCredsInitOpts.RegistryInfo.PullPassword, "registry-pull-password", v.GetString(common.VInitRegistryPullPass), lang.CmdInitFlagRegPullPass)

	// Flags for using an external artifact server
	updateCredsCmd.Flags().StringVar(&updateCredsInitOpts.ArtifactServer.Address, "artifact-url", v.GetString(common.VInitArtifactURL), lang.CmdInitFlagArtifactURL)
	updateCredsCmd.Flags().StringVar(&updateCredsInitOpts.ArtifactServer.PushUsername, "artifact-push-username", v.GetString(common.VInitArtifactPushUser), lang.CmdInitFlagArtifactPushUser)
	updateCredsCmd.Flags().StringVar(&updateCredsInitOpts.ArtifactServer.PushToken, "artifact-push-token", v.GetString(common.VInitArtifactPushToken), lang.CmdInitFlagArtifactPushToken)

	updateCredsCmd.Flags().SortFlags = true

	toolsCmd.AddCommand(clearCacheCmd)
	clearCacheCmd.Flags().StringVar(&config.CommonOptions.CachePath, "zarf-cache", config.ZarfDefaultCachePath, lang.CmdToolsClearCacheFlagCachePath)

	toolsCmd.AddCommand(downloadInitCmd)
	downloadInitCmd.Flags().StringVarP(&outputDirectory, "output-directory", "o", "", lang.CmdToolsDownloadInitFlagOutputDirectory)

	toolsCmd.AddCommand(generatePKICmd)
	generatePKICmd.Flags().StringArrayVar(&subAltNames, "sub-alt-name", []string{}, lang.CmdToolsGenPkiFlagAltName)

	toolsCmd.AddCommand(generateKeyCmd)
}
