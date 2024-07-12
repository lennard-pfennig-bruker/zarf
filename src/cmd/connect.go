// SPDX-License-Identifier: Apache-2.0
// SPDX-FileCopyrightText: 2021-Present The Zarf Authors

// Package cmd contains the CLI commands for Zarf contains the CLI commands for Zarf.
package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/defenseunicorns/zarf/src/config/lang"
	"github.com/defenseunicorns/zarf/src/pkg/cluster"
	"github.com/defenseunicorns/zarf/src/pkg/message"
	"github.com/defenseunicorns/zarf/src/pkg/utils/exec"
)

var (
	cliOnly bool
	zt      cluster.TunnelInfo
)

var connectCmd = &cobra.Command{
	Use:     "connect { REGISTRY | GIT | connect-name }",
	Aliases: []string{"c"},
	Short:   lang.CmdConnectShort,
	Long:    lang.CmdConnectLong,
	RunE: func(cmd *cobra.Command, args []string) error {
		var target string
		if len(args) > 0 {
			target = args[0]
		}
		spinner := message.NewProgressSpinner(lang.CmdConnectPreparingTunnel, target)
		defer spinner.Stop()
		c, err := cluster.NewCluster()
		if err != nil {
			return err
		}

		ctx := cmd.Context()

		var tunnel *cluster.Tunnel
		if target == "" {
			tunnel, err = c.ConnectTunnelInfo(ctx, zt)
		} else {
			var ti cluster.TunnelInfo
			ti, err = c.NewTargetTunnelInfo(ctx, target)
			if err != nil {
				return fmt.Errorf("unable to create tunnel: %w", err)
			}
			if zt.LocalPort != 0 {
				ti.LocalPort = zt.LocalPort
			}
			tunnel, err = c.ConnectTunnelInfo(ctx, ti)
		}

		if err != nil {
			return fmt.Errorf("unable to connect to the service: %w", err)
		}

		defer tunnel.Close()
		url := tunnel.FullURL()

		// Dump the tunnel URL to the console for other tools to use.
		fmt.Print(url)

		if cliOnly {
			spinner.Updatef(lang.CmdConnectEstablishedCLI, url)
		} else {
			spinner.Updatef(lang.CmdConnectEstablishedWeb, url)

			if err := exec.LaunchURL(url); err != nil {
				message.Debug(err)
			}
		}

		// Wait for the interrupt signal or an error.
		select {
		case <-ctx.Done():
			spinner.Successf(lang.CmdConnectTunnelClosed, url)
		case err = <-tunnel.ErrChan():
			return fmt.Errorf("lost connection to the service: %w", err)
		}
		return nil
	},
}

var connectListCmd = &cobra.Command{
	Use:     "list",
	Aliases: []string{"l"},
	Short:   lang.CmdConnectListShort,
	RunE: func(cmd *cobra.Command, _ []string) error {
		c, err := cluster.NewCluster()
		if err != nil {
			return err
		}
		connections, err := c.ListConnections(cmd.Context())
		if err != nil {
			return err
		}
		message.PrintConnectStringTable(connections)
		return nil
	},
}

func init() {
	rootCmd.AddCommand(connectCmd)
	connectCmd.AddCommand(connectListCmd)

	connectCmd.Flags().StringVar(&zt.ResourceName, "name", "", lang.CmdConnectFlagName)
	connectCmd.Flags().StringVar(&zt.Namespace, "namespace", cluster.ZarfNamespaceName, lang.CmdConnectFlagNamespace)
	connectCmd.Flags().StringVar(&zt.ResourceType, "type", cluster.SvcResource, lang.CmdConnectFlagType)
	connectCmd.Flags().IntVar(&zt.LocalPort, "local-port", 0, lang.CmdConnectFlagLocalPort)
	connectCmd.Flags().IntVar(&zt.RemotePort, "remote-port", 0, lang.CmdConnectFlagRemotePort)
	connectCmd.Flags().BoolVar(&cliOnly, "cli-only", false, lang.CmdConnectFlagCliOnly)
}
