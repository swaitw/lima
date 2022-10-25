package main

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/AlecAivazis/survey/v2"
	"github.com/containerd/containerd/identifiers"
	"github.com/lima-vm/lima/cmd/limactl/guessarg"
	"github.com/lima-vm/lima/pkg/editutil"
	"github.com/lima-vm/lima/pkg/ioutilx"
	"github.com/lima-vm/lima/pkg/limayaml"
	networks "github.com/lima-vm/lima/pkg/networks/reconcile"
	"github.com/lima-vm/lima/pkg/osutil"
	"github.com/lima-vm/lima/pkg/start"
	"github.com/lima-vm/lima/pkg/store"
	"github.com/lima-vm/lima/pkg/store/filenames"
	"github.com/lima-vm/lima/pkg/templatestore"
	"github.com/mattn/go-isatty"
	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
)

func newStartCommand() *cobra.Command {
	var startCommand = &cobra.Command{
		Use: "start NAME|FILE.yaml|URL",
		Example: `
To create an instance "default" (if not created yet) from the default Ubuntu template, and start it:
$ limactl start

To create an instance "default" from a template "docker":
$ limactl start --name=default template://docker

To see the template list:
$ limactl start --list-templates

To create an instance "default" from a local file:
$ limactl start --name=default /usr/local/share/lima/examples/fedora.yaml

To create an instance "default" from a remote URL (use carefully, with a trustable source):
$ limactl start --name=default https://raw.githubusercontent.com/lima-vm/lima/master/examples/alpine.yaml
`,
		Short:             "Start an instance of Lima",
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: startBashComplete,
		RunE:              startAction,
	}
	// TODO: "survey" does not support using cygwin terminal on windows yet
	startCommand.Flags().Bool("tty", isatty.IsTerminal(os.Stdout.Fd()), "enable TUI interactions such as opening an editor, defaults to true when stdout is a terminal")
	startCommand.Flags().String("name", "", "override the instance name")
	startCommand.Flags().Bool("list-templates", false, "list available templates and exit")
	return startCommand
}

func loadOrCreateInstance(cmd *cobra.Command, args []string) (*store.Instance, error) {
	var arg string // can be empty
	if len(args) > 0 {
		arg = args[0]
	}

	var (
		st  = &creatorState{}
		err error
	)
	st.instName, err = cmd.Flags().GetString("name")
	if err != nil {
		return nil, err
	}
	const yBytesLimit = 4 * 1024 * 1024 // 4MiB

	if ok, u := guessarg.SeemsTemplateURL(arg); ok {
		// No need to use SecureJoin here. https://github.com/lima-vm/lima/pull/805#discussion_r853411702
		templateName := filepath.Join(u.Host, u.Path)
		logrus.Debugf("interpreting argument %q as a template name %q", arg, templateName)
		if st.instName == "" {
			// e.g., templateName = "deprecated/centos-7" , st.instName = "centos-7"
			st.instName = filepath.Base(templateName)
		}
		st.yBytes, err = templatestore.Read(templateName)
		if err != nil {
			return nil, err
		}
	} else if guessarg.SeemsHTTPURL(arg) {
		if st.instName == "" {
			st.instName, err = guessarg.InstNameFromURL(arg)
			if err != nil {
				return nil, err
			}
		}
		logrus.Debugf("interpreting argument %q as a http url for instance %q", arg, st.instName)
		resp, err := http.Get(arg)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		st.yBytes, err = ioutilx.ReadAtMaximum(resp.Body, yBytesLimit)
		if err != nil {
			return nil, err
		}
	} else if guessarg.SeemsFileURL(arg) {
		if st.instName == "" {
			st.instName, err = guessarg.InstNameFromURL(arg)
			if err != nil {
				return nil, err
			}
		}
		logrus.Debugf("interpreting argument %q as a file url for instance %q", arg, st.instName)
		r, err := os.Open(strings.TrimPrefix(arg, "file://"))
		if err != nil {
			return nil, err
		}
		defer r.Close()
		st.yBytes, err = ioutilx.ReadAtMaximum(r, yBytesLimit)
		if err != nil {
			return nil, err
		}
	} else if guessarg.SeemsYAMLPath(arg) {
		if st.instName == "" {
			st.instName, err = guessarg.InstNameFromYAMLPath(arg)
			if err != nil {
				return nil, err
			}
		}
		logrus.Debugf("interpreting argument %q as a file path for instance %q", arg, st.instName)
		r, err := os.Open(arg)
		if err != nil {
			return nil, err
		}
		defer r.Close()
		st.yBytes, err = ioutilx.ReadAtMaximum(r, yBytesLimit)
		if err != nil {
			return nil, err
		}
	} else {
		if arg == "" {
			if st.instName == "" {
				st.instName = DefaultInstanceName
			}
		} else {
			logrus.Debugf("interpreting argument %q as an instance name", arg)
			if st.instName != "" && st.instName != arg {
				return nil, fmt.Errorf("instance name %q and CLI flag --name=%q cannot be specified together", arg, st.instName)
			}
			st.instName = arg
		}
		if err := identifiers.Validate(st.instName); err != nil {
			return nil, fmt.Errorf("argument must be either an instance name, a YAML file path, or a URL, got %q: %w", st.instName, err)
		}
		inst, err := store.Inspect(st.instName)
		if err == nil {
			logrus.Infof("Using the existing instance %q", st.instName)
			if arg == "" {
				logrus.Infof("Hint: To create another instance, run the following command: limactl start --name=NAME template://default")
			}
			return inst, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		if arg != "" && arg != DefaultInstanceName {
			logrus.Infof("Creating an instance %q from template://default (Not from template://%s)", st.instName, st.instName)
			logrus.Warnf("This form is deprecated. Use `limactl start --name=%s template://default` instead", st.instName)
		}
		// Read the default template for creating a new instance
		st.yBytes, err = templatestore.Read(templatestore.Default)
		if err != nil {
			return nil, err
		}
	}

	// Create an instance, with menu TUI when TTY is available
	tty, err := cmd.Flags().GetBool("tty")
	if err != nil {
		return nil, err
	}
	if tty {
		var err error
		st, err = chooseNextCreatorState(st)
		if err != nil {
			return nil, err
		}
	} else {
		logrus.Info("Terminal is not available, proceeding without opening an editor")
	}
	saveBrokenEditorBuffer := tty
	return createInstance(st, saveBrokenEditorBuffer)
}

func createInstance(st *creatorState, saveBrokenEditorBuffer bool) (*store.Instance, error) {
	if st.instName == "" {
		return nil, errors.New("got empty st.instName")
	}
	if len(st.yBytes) == 0 {
		return nil, errors.New("got empty st.yBytes")
	}

	instDir, err := store.InstanceDir(st.instName)
	if err != nil {
		return nil, err
	}

	// the full path of the socket name must be less than UNIX_PATH_MAX chars.
	maxSockName := filepath.Join(instDir, filenames.LongestSock)
	if len(maxSockName) >= osutil.UnixPathMax {
		return nil, fmt.Errorf("instance name %q too long: %q must be less than UNIX_PATH_MAX=%d characters, but is %d",
			st.instName, maxSockName, osutil.UnixPathMax, len(maxSockName))
	}
	if _, err := os.Stat(instDir); !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("instance %q already exists (%q)", st.instName, instDir)
	}
	// limayaml.Load() needs to pass the store file path to limayaml.FillDefault() to calculate default MAC addresses
	filePath := filepath.Join(instDir, filenames.LimaYAML)
	y, err := limayaml.Load(st.yBytes, filePath)
	if err != nil {
		return nil, err
	}
	if err := limayaml.Validate(*y, true); err != nil {
		if !saveBrokenEditorBuffer {
			return nil, err
		}
		rejectedYAML := "lima.REJECTED.yaml"
		if writeErr := os.WriteFile(rejectedYAML, st.yBytes, 0644); writeErr != nil {
			return nil, fmt.Errorf("the YAML is invalid, attempted to save the buffer as %q but failed: %v: %w", rejectedYAML, writeErr, err)
		}
		return nil, fmt.Errorf("the YAML is invalid, saved the buffer as %q: %w", rejectedYAML, err)
	}
	if err := os.MkdirAll(instDir, 0700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filePath, st.yBytes, 0644); err != nil {
		return nil, err
	}
	return store.Inspect(st.instName)
}

type creatorState struct {
	instName string // instance name
	yBytes   []byte // yaml bytes
}

func chooseNextCreatorState(st *creatorState) (*creatorState, error) {
	for {
		var ans string
		prompt := &survey.Select{
			Message: fmt.Sprintf("Creating an instance %q", st.instName),
			Options: []string{
				"Proceed with the current configuration",
				"Open an editor to review or modify the current configuration",
				"Choose another example (docker, podman, archlinux, fedora, ...)",
				"Exit",
			},
		}
		if err := survey.AskOne(prompt, &ans); err != nil {
			logrus.WithError(err).Warn("Failed to open TUI")
			return st, nil
		}
		switch ans {
		case prompt.Options[0]: // "Proceed with the current configuration"
			return st, nil
		case prompt.Options[1]: // "Open an editor ..."
			hdr := fmt.Sprintf("# Review and modify the following configuration for Lima instance %q.\n", st.instName)
			if st.instName == DefaultInstanceName {
				hdr += "# - In most cases, you do not need to modify this file.\n"
			}
			hdr += "# - To cancel starting Lima, just save this file as an empty file.\n"
			hdr += "\n"
			hdr += editutil.GenerateEditorWarningHeader()
			var err error
			st.yBytes, err = editutil.OpenEditor(st.instName, st.yBytes, hdr)
			if err != nil {
				return st, err
			}
			if len(st.yBytes) == 0 {
				logrus.Info("Aborting, as requested by saving the file with empty content")
				os.Exit(0)
				return st, errors.New("should not reach here")
			}
			return st, nil
		case prompt.Options[2]: // "Choose another example..."
			examples, err := templatestore.Templates()
			if err != nil {
				return st, err
			}
			var ansEx int
			promptEx := &survey.Select{
				Message: "Choose an example",
				Options: make([]string, len(examples)),
			}
			for i := range examples {
				promptEx.Options[i] = examples[i].Name
			}
			if err := survey.AskOne(promptEx, &ansEx); err != nil {
				return st, err
			}
			if ansEx > len(examples)-1 {
				return st, fmt.Errorf("invalid answer %d for %d entries", ansEx, len(examples))
			}
			yamlPath := examples[ansEx].Location
			st.instName, err = guessarg.InstNameFromYAMLPath(yamlPath)
			if err != nil {
				return nil, err
			}
			st.yBytes, err = os.ReadFile(yamlPath)
			if err != nil {
				return nil, err
			}
			continue
		case prompt.Options[3]: // "Exit"
			os.Exit(0)
			return st, errors.New("should not reach here")
		default:
			return st, fmt.Errorf("unexpected answer %q", ans)
		}
	}
}

func startAction(cmd *cobra.Command, args []string) error {
	if listTemplates, err := cmd.Flags().GetBool("list-templates"); err != nil {
		return err
	} else if listTemplates {
		if templates, err := templatestore.Templates(); err == nil {
			w := cmd.OutOrStdout()
			for _, f := range templates {
				fmt.Fprintln(w, f.Name)
			}
			return nil
		}
	}

	inst, err := loadOrCreateInstance(cmd, args)
	if err != nil {
		return err
	}
	if len(inst.Errors) > 0 {
		return fmt.Errorf("errors inspecting instance: %+v", inst.Errors)
	}
	switch inst.Status {
	case store.StatusRunning:
		logrus.Infof("The instance %q is already running. Run `%s` to open the shell.",
			inst.Name, start.LimactlShellCmd(inst.Name))
		// Not an error
		return nil
	case store.StatusStopped:
		// NOP
	default:
		logrus.Warnf("expected status %q, got %q", store.StatusStopped, inst.Status)
	}
	ctx := cmd.Context()
	err = networks.Reconcile(ctx, inst.Name)
	if err != nil {
		return err
	}
	return start.Start(ctx, inst)
}

func startBashComplete(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	comp, _ := bashCompleteInstanceNames(cmd)
	if templates, err := templatestore.Templates(); err == nil {
		for _, f := range templates {
			comp = append(comp, "template://"+f.Name)
		}
	}
	return comp, cobra.ShellCompDirectiveDefault
}
