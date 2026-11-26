package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/agent-mem/agent-mem/internal/hooks"
)

func newInstallOmOCmd() *cobra.Command {
	var opts hooks.InstallOptions
	cmd := &cobra.Command{
		Use:   "omo",
		Short: "Install the agent-mem OmO hook extension",
		Long: `Install the hook extension only, equivalent to install-hooks omo.

User scope writes extensions/agent-mem.ts under the OmO agent directory
(OMO_CODING_AGENT_DIR, SENPI_CODING_AGENT_DIR, PI_CODING_AGENT_DIR, or
~/.omo/agent, in that order). For custom profiles, use --hooks-file.
Project scope writes .omo/extensions/agent-mem.ts under the project root;
OmO only loads project extensions in trusted projects.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			result, err := hooks.InstallHooksWithOptions(hooks.ProviderOmO, opts)
			if err != nil {
				return err
			}
			if result.Changed {
				fmt.Fprintf(cmd.OutOrStdout(), "installed agent-mem omo hook in %s\n", result.Path)
			} else {
				fmt.Fprintf(cmd.OutOrStdout(), "agent-mem omo hook already installed in %s\n", result.Path)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&opts.Scope, "scope", "user", "Install scope: user or project")
	cmd.Flags().StringVar(&opts.ProjectDir, "project-dir", "", "Project root for project scope (defaults to the current working directory)")
	cmd.Flags().StringVar(&opts.HooksPath, "hooks-file", "", "Override the extension destination file")
	return cmd
}
