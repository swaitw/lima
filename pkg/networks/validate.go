package networks

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/lima-vm/lima/pkg/osutil"
)

func (config *YAML) Validate() error {
	// validate all paths.* values
	paths := reflect.ValueOf(&config.Paths).Elem()
	pathsMap := make(map[string]string, paths.NumField())
	var socketVMNetNotFound, vdeVMNetNotFound, vdeSwitchNotFound bool
	for i := 0; i < paths.NumField(); i++ {
		// extract YAML name from struct tag; strip options like "omitempty"
		name := paths.Type().Field(i).Tag.Get("yaml")
		if i := strings.IndexRune(name, ','); i > -1 {
			name = name[:i]
		}
		path := paths.Field(i).Interface().(string)
		pathsMap[name] = path
		// varPath will be created securely, but any existing parent directories must already be secure
		if name == "varRun" {
			path = findBaseDirectory(path)
		}
		err := validatePath(path, name == "varRun")
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				switch name {
				// sudoers file does not need to exist; otherwise `limactl sudoers` couldn't bootstrap
				case "sudoers":
					continue
				case "socketVMNet":
					socketVMNetNotFound = true
					continue
				case "vdeVMNet":
					vdeVMNetNotFound = true
					continue
				case "vdeSwitch":
					vdeSwitchNotFound = true
					continue
				}
			}
			return fmt.Errorf("networks.yaml field `paths.%s` error: %w", name, err)
		}
	}
	if socketVMNetNotFound && vdeVMNetNotFound {
		return fmt.Errorf("networks.yaml: either %q (`paths.socketVMNet`) or %q (`paths.vdeVMNet`) has to be installed", pathsMap["socketVMNet"], pathsMap["vdeVMNet"])
	}
	if socketVMNetNotFound && !vdeVMNetNotFound && vdeSwitchNotFound {
		return fmt.Errorf("networks.yaml: %q (`paths.vdeVMNet`) requires %q (`paths.vdeSwitch`) to be installed", pathsMap["vdeVMNet"], pathsMap["vdeSwitch"])
	}
	// TODO(jandubois): validate network definitions
	return nil
}

// findBaseDirectory removes non-existing directories from the end of the path.
func findBaseDirectory(path string) string {
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		if path != "/" {
			return findBaseDirectory(filepath.Dir(path))
		}
	}
	return path
}

func validatePath(path string, allowDaemonGroupWritable bool) error {
	if path == "" {
		return nil
	}
	if path[0] != '/' {
		return fmt.Errorf("path %q is not an absolute path", path)
	}
	if strings.ContainsRune(path, ' ') {
		return fmt.Errorf("path %q contains whitespace", path)
	}
	fi, err := os.Lstat(path)
	if err != nil {
		return err
	}
	file := "file"
	if fi.Mode().IsDir() {
		file = "dir"
	}
	// TODO: should we allow symlinks when both the link and the target are secure?
	// E.g. on macOS /var is a symlink to /private/var, /etc to /private/etc
	if (fi.Mode() & fs.ModeSymlink) != 0 {
		return fmt.Errorf("%s %q is a symlink", file, path)
	}
	stat, ok := osutil.SysStat(fi)
	if !ok {
		// should never happen
		return fmt.Errorf("could not retrieve stat buffer for %q", path)
	}
	root, err := osutil.LookupUser("root")
	if err != nil {
		return err
	}
	if stat.Uid != root.Uid {
		return fmt.Errorf(`%s %q is not owned by %q (uid: %d), but by uid %d`, file, path, root.User, root.Uid, stat.Uid)
	}
	if allowDaemonGroupWritable {
		daemon, err := osutil.LookupUser("daemon")
		if err != nil {
			return err
		}
		if fi.Mode()&020 != 0 && stat.Gid != root.Gid && stat.Gid != daemon.Gid {
			return fmt.Errorf(`%s %q is group-writable and group is neither %q (gid: %d) nor %q (gid: %d), but is gid: %d`,
				file, path, root.User, root.Gid, daemon.User, daemon.Gid, stat.Gid)
		}
		if fi.Mode().IsDir() && fi.Mode()&1 == 0 && (fi.Mode()&0010 == 0 || stat.Gid != daemon.Gid) {
			return fmt.Errorf(`%s %q is not executable by the %q (gid: %d)" group`, file, path, daemon.User, daemon.Gid)
		}
	} else if fi.Mode()&020 != 0 && stat.Gid != root.Gid {
		return fmt.Errorf(`%s %q is group-writable and group is not %q (gid: %d), but is gid: %d`,
			file, path, root.User, root.Gid, stat.Gid)
	}
	if fi.Mode()&002 != 0 {
		return fmt.Errorf("%s %q is world-writable", file, path)
	}
	if path != "/" {
		return validatePath(filepath.Dir(path), allowDaemonGroupWritable)
	}
	return nil
}
