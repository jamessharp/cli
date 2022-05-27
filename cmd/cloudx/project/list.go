package project

import (
	"fmt"

	"github.com/ory/cli/cmd/cloudx/client"

	"github.com/spf13/cobra"

	"github.com/ory/x/cmdx"
)

func NewListProjectsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "projects",
		Short: fmt.Sprintf("List your Ory Cloud projects"),
		RunE: func(cmd *cobra.Command, args []string) error {
			h, err := client.NewCommandHelper(cmd)
			if err != nil {
				return err
			}

			projects, err := h.ListProjects()
			if err != nil {
				return cmdx.PrintOpenAPIError(cmd, err)
			}

			cmdx.PrintTable(cmd, &outputProjectCollection{projects})
			return nil
		},
	}

	return cmd
}