package qemu

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"

	"github.com/AkihiroSuda/lima/pkg/downloader"
	"github.com/AkihiroSuda/lima/pkg/iso9660util"
	"github.com/AkihiroSuda/lima/pkg/limayaml"
	"github.com/AkihiroSuda/lima/pkg/store/filenames"
	"github.com/docker/go-units"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

type Config struct {
	Name        string
	InstanceDir string
	LimaYAML    *limayaml.LimaYAML
}

func EnsureDisk(cfg Config) error {
	diffDisk := filepath.Join(cfg.InstanceDir, filenames.DiffDisk)
	if _, err := os.Stat(diffDisk); err == nil || !errors.Is(err, os.ErrNotExist) {
		// disk is already ensured
		return err
	}

	baseDisk := filepath.Join(cfg.InstanceDir, filenames.BaseDisk)
	if _, err := os.Stat(baseDisk); errors.Is(err, os.ErrNotExist) {
		var ensuredBaseDisk bool
		errs := make([]error, len(cfg.LimaYAML.Images))
		for i, f := range cfg.LimaYAML.Images {
			if f.Arch != cfg.LimaYAML.Arch {
				errs[i] = fmt.Errorf("unsupported arch: %q", f.Arch)
				continue
			}
			logrus.Infof("Attempting to download the image from %q", f.Location)
			res, err := downloader.Download(baseDisk, f.Location, downloader.WithCache())
			if err != nil {
				errs[i] = errors.Wrapf(err, "failed to download %q", f.Location)
				continue
			}
			switch res.Status {
			case downloader.StatusDownloaded:
				logrus.Infof("Downloaded image from %q", f.Location)
			case downloader.StatusUsedCache:
				logrus.Infof("Using cache %q", res.CachePath)
			default:
				logrus.Warnf("Unexpected result from downloader.Download(): %+v", res)
			}
			ensuredBaseDisk = true
			break
		}
		if !ensuredBaseDisk {
			return errors.Errorf("failed to download the image, attempted %d candidates, errors=%v",
				len(cfg.LimaYAML.Images), errs)
		}
	}
	diskSize, _ := units.RAMInBytes(cfg.LimaYAML.Disk)
	if diskSize == 0 {
		return nil
	}
	isBaseDiskISO, err := iso9660util.IsISO9660(baseDisk)
	if err != nil {
		return err
	}
	args := []string{"create", "-f", "qcow2"}
	if !isBaseDiskISO {
		args = append(args, "-b", baseDisk)
	}
	args = append(args, diffDisk, strconv.Itoa(int(diskSize)))
	cmd := exec.Command("qemu-img", args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return errors.Wrapf(err, "failed to run %v: %q", cmd.Args, string(out))
	}
	return nil
}

func Cmdline(cfg Config) (string, []string, error) {
	y := cfg.LimaYAML
	exeBase := "qemu-system-" + y.Arch
	exe, err := exec.LookPath(exeBase)
	if err != nil {
		return "", nil, err
	}
	var args []string

	// Architecture
	accel := getAccel(y.Arch)
	switch y.Arch {
	case limayaml.X8664:
		// NOTE: "-cpu host" seems to cause kernel panic
		// (MacBookPro 2020, Intel(R) Core(TM) i7-1068NG7 CPU @ 2.30GHz, macOS 11.3, Ubuntu 21.04)
		args = append(args, "-cpu", "Haswell-v4")
		args = append(args, "-machine", "q35,accel="+accel)
	case limayaml.AARCH64:
		args = append(args, "-cpu", "cortex-a72")
		args = append(args, "-machine", "virt,accel="+accel+",highmem=off")
	}

	// SMP
	args = append(args, "-smp",
		fmt.Sprintf("%d,sockets=1,cores=%d,threads=1", y.CPUs, y.CPUs))

	// Memory
	memBytes, err := units.RAMInBytes(y.Memory)
	if err != nil {
		return "", nil, err
	}
	args = append(args, "-m", strconv.Itoa(int(memBytes>>20)))

	// Firmware
	if !y.Firmware.LegacyBIOS {
		firmware, err := getFirmware(exe, y.Arch)
		if err != nil {
			return "", nil, err
		}
		args = append(args, "-drive", fmt.Sprintf("if=pflash,format=raw,readonly,file=%s", firmware))
	} else if y.Arch != limayaml.X8664 {
		logrus.Warnf("field `firmware.legacyBIOS` is not supported for architecture %q, ignoring", y.Arch)
	}

	baseDisk := filepath.Join(cfg.InstanceDir, filenames.BaseDisk)
	diffDisk := filepath.Join(cfg.InstanceDir, filenames.DiffDisk)
	isBaseDiskCDROM, err := iso9660util.IsISO9660(baseDisk)
	if err != nil {
		return "", nil, err
	}
	if isBaseDiskCDROM {
		args = append(args, "-boot", "order=d,splash-time=0,menu=on")
		args = append(args, "-drive", fmt.Sprintf("file=%s,media=cdrom,readonly=on", baseDisk))
	} else {
		args = append(args, "-boot", "order=c,splash-time=0,menu=on")
	}
	if diskSize, _ := units.RAMInBytes(cfg.LimaYAML.Disk); diskSize > 0 {
		args = append(args, "-drive", fmt.Sprintf("file=%s,if=virtio", diffDisk))
	} else if !isBaseDiskCDROM {
		args = append(args, "-drive", fmt.Sprintf("file=%s,if=virtio", baseDisk))
	}
	// cloud-init
	args = append(args, "-cdrom", filepath.Join(cfg.InstanceDir, filenames.CIDataISO))

	// Network
	// CIDR is intentionally hardcoded to 192.168.5.0/24, as each of QEMU has its own independent slirp network.
	// TODO: enable bridge (with sudo?)
	args = append(args, "-net", "nic,model=virtio")
	args = append(args, "-net", fmt.Sprintf("user,net=192.168.5.0/24,hostfwd=tcp:127.0.0.1:%d-:22", y.SSH.LocalPort))

	// virtio-rng-pci acceralates starting up the OS, according to https://wiki.gentoo.org/wiki/QEMU/Options
	args = append(args, "-device", "virtio-rng-pci")

	// Graphics
	if y.Video.Display != "" {
		args = append(args, "-display", y.Video.Display)
	}
	switch y.Arch {
	case limayaml.X8664:
		args = append(args, "-device", "virtio-vga")
		args = append(args, "-device", "virtio-keyboard-pci")
		args = append(args, "-device", "virtio-mouse-pci")
	default:
		// QEMU does not seem to support virtio-vga for aarch64
		args = append(args, "-vga", "none", "-device", "ramfb")
		args = append(args, "-device", "usb-ehci")
		args = append(args, "-device", "usb-kbd")
		args = append(args, "-device", "usb-mouse")
	}

	// Parallel
	args = append(args, "-parallel", "none")

	// Serial
	serialSock := filepath.Join(cfg.InstanceDir, filenames.SerialSock)
	if err := os.RemoveAll(serialSock); err != nil {
		return "", nil, err
	}
	serialLog := filepath.Join(cfg.InstanceDir, filenames.SerialLog)
	if err := os.RemoveAll(serialLog); err != nil {
		return "", nil, err
	}
	const serialChardev = "char-serial"
	args = append(args, "-chardev", fmt.Sprintf("socket,id=%s,path=%s,server,nowait,logfile=%s", serialChardev, serialSock, serialLog))
	args = append(args, "-serial", "chardev:"+serialChardev)

	// We also want to enable vsock and virtfs here, but QEMU does not support vsock and virtfs for macOS hosts

	// QMP
	qmpSock := filepath.Join(cfg.InstanceDir, filenames.QMPSock)
	if err := os.RemoveAll(qmpSock); err != nil {
		return "", nil, err
	}
	const qmpChardev = "char-qmp"
	args = append(args, "-chardev", fmt.Sprintf("socket,id=%s,path=%s,server,nowait", qmpChardev, qmpSock))
	args = append(args, "-qmp", "chardev:"+qmpChardev)

	// QEMU process
	args = append(args, "-name", "lima-"+cfg.Name)
	args = append(args, "-pidfile", filepath.Join(cfg.InstanceDir, filenames.QemuPID))

	return exe, args, nil
}

func getAccel(arch limayaml.Arch) string {
	nativeX8664 := arch == limayaml.X8664 && runtime.GOARCH == "amd64"
	nativeAARCH64 := arch == limayaml.AARCH64 && runtime.GOARCH == "arm64"
	native := nativeX8664 || nativeAARCH64
	if native {
		switch runtime.GOOS {
		case "darwin":
			return "hvf"
		case "linux":
			return "kvm"
		case "netbsd":
			return "nvmm" // untested
		case "windows":
			return "whpx" // untested
		}
	}
	return "tcg"
}

func getFirmware(qemuExe string, arch limayaml.Arch) (string, error) {
	binDir := filepath.Dir(qemuExe)  // "/usr/local/bin"
	localDir := filepath.Dir(binDir) // "/usr/local"

	candidates := []string{
		filepath.Join(localDir, fmt.Sprintf("share/qemu/edk2-%s-code.fd", arch)), // macOS (homebrew)
	}

	switch arch {
	case limayaml.X8664:
		// Debian package "ovmf"
		candidates = append(candidates, "/usr/share/OVMF/OVMF_CODE.fd")
	case limayaml.AARCH64:
		// Debian package "qemu-efi-aarch64"
		candidates = append(candidates, "/usr/share/qemu-efi-aarch64/QEMU_EFI.fd")
	}

	logrus.Debugf("firmware candidates = %v", candidates)

	for _, f := range candidates {
		if _, err := os.Stat(f); err == nil {
			return f, nil
		}
	}

	if arch == limayaml.X8664 {
		return "", errors.Errorf("could not find firmware for %q (hint: try setting `firmware.legacyBIOS` to `true`)", qemuExe)
	}
	return "", errors.Errorf("could not find firmware for %q", qemuExe)
}
