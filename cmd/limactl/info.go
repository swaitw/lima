package main

import (
	"encoding/json"
	"fmt"

	"github.com/lima-vm/lima/pkg/infoutil"
	"github.com/spf13/cobra"
)

func newInfoCommand() *cobra.Command {
	infoCommand := &cobra.Command{
		Use:   "info",
		Short: "Show diagnostic information",
		Args:  cobra.NoArgs,
		RunE:  infoAction,
	}
	return infoCommand
}

func infoAction(cmd *cobra.Command, args []string) error {
	info, err := infoutil.GetInfo()
	if err != nil {
		return err
	}
	j, err := json.MarshalIndent(info, "", "    ")
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(cmd.OutOrStdout(), string(j))
	return err
}
