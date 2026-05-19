package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/fsnotify/fsnotify"
	flag "github.com/spf13/pflag"
	"golang.org/x/sys/unix"
)

var (
	version = "dev"
	verbose bool
)

func logv(format string, args ...any) {
	if verbose {
		fmt.Fprintf(os.Stderr, format+"\n", args...)
	}
}

const (
	EV_SYN = 0x00
	EV_KEY = 0x01
	EV_ABS = 0x03

	ABS_X        = 0x00
	ABS_Y        = 0x01
	ABS_PRESSURE = 0x18
	ABS_DISTANCE = 0x19
	ABS_TILT_X   = 0x1a
	ABS_TILT_Y   = 0x1b

	EVIOCGRAB = 0x40044590

	UI_DEV_SETUP   = 0x405c5503
	UI_DEV_CREATE  = 0x5501
	UI_DEV_DESTROY = 0x5502
	UI_SET_EVBIT   = 0x40045564
	UI_SET_KEYBIT  = 0x40045565
	UI_SET_ABSBIT  = 0x40045567
)

var keyNames = map[string]uint16{
	"BTN_TOOL_PEN":    0x140,
	"BTN_TOOL_RUBBER": 0x141,
	"BTN_TOUCH":       0x14a,
	"BTN_STYLUS":      0x14b,
	"BTN_STYLUS2":     0x14c,
	"TOOL_PEN":        0x140,
	"TOOL_RUBBER":     0x141,
	"TOUCH":           0x14a,
	"STYLUS":          0x14b,
	"STYLUS2":         0x14c,
}

type uinputSetup struct {
	ID    [4]uint16
	Name  [80]byte
	FFMax uint32
}

type uinputAbsSetup struct {
	Code    uint16
	_       [2]byte
	AbsInfo absInfo
}

type absInfo struct {
	Value      int32
	Minimum    int32
	Maximum    int32
	Fuzz       int32
	Flat       int32
	Resolution int32
}

func ioctl(fd int, req uint, arg uintptr) error {
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), uintptr(req), arg)
	if errno != 0 {
		return errno
	}
	return nil
}

func ioctlInt(fd int, req uint, val int) error {
	return ioctl(fd, req, uintptr(val))
}

func getAbsInfo(fd int, axis uint16) (absInfo, error) {
	var ai absInfo
	req := uint(0x80184540 + uint(axis))
	err := ioctl(fd, req, uintptr(unsafe.Pointer(&ai)))
	return ai, err
}

func setupUinputAbs(fd int, code uint16, ai absInfo) error {
	setup := uinputAbsSetup{
		Code:    code,
		AbsInfo: ai,
	}
	req := uint(0x401c5504)
	return ioctl(fd, req, uintptr(unsafe.Pointer(&setup)))
}

func openRealDevice(devPath string) (int, error) {
	resolved, err := filepath.EvalSymlinks(devPath)
	if err != nil {
		return -1, err
	}
	base := filepath.Base(resolved)
	sysDevPath := "/sys/class/input/" + base + "/dev"

	data, err := os.ReadFile(sysDevPath)
	if err != nil {
		return -1, fmt.Errorf("no sysfs entry for %s: %w", base, err)
	}
	parts := strings.Split(strings.TrimSpace(string(data)), ":")
	if len(parts) != 2 {
		return -1, fmt.Errorf("unexpected dev format: %s", string(data))
	}
	major := 0
	minor := 0
	fmt.Sscanf(parts[0], "%d", &major)
	fmt.Sscanf(parts[1], "%d", &minor)

	tmpDev := "/dev/.stylus-remapper-dev"
	os.Remove(tmpDev)
	dev := unix.Mkdev(uint32(major), uint32(minor))
	if err := unix.Mknod(tmpDev, unix.S_IFCHR|0600, int(dev)); err != nil {
		return -1, fmt.Errorf("mknod: %w", err)
	}
	fd, err := unix.Open(tmpDev, unix.O_RDONLY, 0)
	os.Remove(tmpDev)
	if err != nil {
		return -1, fmt.Errorf("open: %w", err)
	}
	return fd, nil
}

func findUinputEventPath(uifd int) (string, error) {
	var sysname [64]byte
	req := uint(0x8040552c) // UI_GET_SYSNAME(64)
	if err := ioctl(uifd, req, uintptr(unsafe.Pointer(&sysname[0]))); err != nil {
		return "", fmt.Errorf("UI_GET_SYSNAME: %v", err)
	}
	name := string(sysname[:])
	if idx := strings.IndexByte(name, 0); idx >= 0 {
		name = name[:idx]
	}
	matches, _ := filepath.Glob(fmt.Sprintf("/sys/devices/virtual/input/%s/event*/dev", name))
	if len(matches) == 0 {
		return "", fmt.Errorf("no event device for %s", name)
	}
	eventName := filepath.Base(filepath.Dir(matches[0]))
	return "/dev/input/" + eventName, nil
}

func parseMapping(s string) (from, to uint16, err error) {
	parts := strings.SplitN(s, "=", 2)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("invalid mapping %q, expected FROM=TO", s)
	}
	fromCode, ok := keyNames[strings.ToUpper(parts[0])]
	if !ok {
		return 0, 0, fmt.Errorf("unknown key name %q", parts[0])
	}
	toCode, ok := keyNames[strings.ToUpper(parts[1])]
	if !ok {
		return 0, 0, fmt.Errorf("unknown key name %q", parts[1])
	}
	return fromCode, toCode, nil
}

const defaultConfigPath = "/home/root/.config/stylus-remapper.conf"

type config struct {
	PressureGamma float64  `json:"pressure-gamma"`
	OffsetX       int      `json:"offset-x"`
	OffsetY       int      `json:"offset-y"`
	Mappings      []string `json:"mappings"`
}

type configFile struct {
	config
	Active  string            `json:"active"`
	Presets map[string]config `json:"presets"`
}

func loadConfig(path string) (config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return config{}, err
	}
	data = bytes.TrimSpace(data)
	if len(data) == 0 {
		return config{}, nil
	}
	var cf configFile
	if err := json.Unmarshal(data, &cf); err != nil {
		return config{}, fmt.Errorf("parse %s: %w", path, err)
	}
	if cf.Active != "" && cf.Presets != nil {
		if preset, ok := cf.Presets[cf.Active]; ok {
			return preset, nil
		}
		return config{}, fmt.Errorf("preset %q not found in %s", cf.Active, path)
	}
	return cf.config, nil
}

type runtimeConfig struct {
	remapTable  map[uint16]uint16
	pressureLUT []int32
	offsetX     int32
	offsetY     int32
}

func buildRuntimeConfig(cfg config, pressureMax int32) (*runtimeConfig, error) {
	rc := &runtimeConfig{
		remapTable: make(map[uint16]uint16),
		offsetX:    int32(cfg.OffsetX),
		offsetY:    int32(cfg.OffsetY),
	}

	for _, m := range cfg.Mappings {
		from, to, err := parseMapping(m)
		if err != nil {
			return nil, err
		}
		rc.remapTable[from] = to
	}

	gamma := cfg.PressureGamma
	if gamma != 0 && gamma != 1.0 && pressureMax > 0 {
		rc.pressureLUT = make([]int32, pressureMax+1)
		fmax := float64(pressureMax)
		for i := int32(0); i <= pressureMax; i++ {
			rc.pressureLUT[i] = int32(math.Round(fmax * math.Pow(float64(i)/fmax, gamma)))
		}
	}

	return rc, nil
}

func watchConfig(path string, pressureMax int32, rtCfg *atomic.Pointer[runtimeConfig]) {
	dir := filepath.Dir(path)

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		fmt.Fprintf(os.Stderr, "config watch failed: %v\n", err)
		return
	}

	if err := watcher.Add(dir); err != nil {
		fmt.Fprintf(os.Stderr, "config watch failed: %v\n", err)
		watcher.Close()
		return
	}

	base := filepath.Base(path)
	for {
		select {
		case event, ok := <-watcher.Events:
			if !ok {
				return
			}
			if filepath.Base(event.Name) != base {
				continue
			}
			if event.Op&(fsnotify.Write|fsnotify.Create) == 0 {
				continue
			}
			time.Sleep(50 * time.Millisecond)
			cfg, err := loadConfig(path)
			if err != nil {
				fmt.Fprintf(os.Stderr, "config reload error: %v\n", err)
				continue
			}
			rc, err := buildRuntimeConfig(cfg, pressureMax)
			if err != nil {
				fmt.Fprintf(os.Stderr, "config reload error: %v\n", err)
				continue
			}
			rtCfg.Store(rc)
			logv("config reloaded")
		case err, ok := <-watcher.Errors:
			if !ok {
				return
			}
			fmt.Fprintf(os.Stderr, "config watch error: %v\n", err)
		}
	}
}

func getDeviceID(fd int) [4]uint16 {
	var id [4]uint16
	req := uint(0x80084502) // EVIOCGID
	ioctl(fd, req, uintptr(unsafe.Pointer(&id[0])))
	return id
}

func getDeviceName(fd int) string {
	var name [256]byte
	req := uint(0x80ff4506) // EVIOCGNAME(256)
	if err := ioctl(fd, req, uintptr(unsafe.Pointer(&name[0]))); err != nil {
		return "Elan marker input"
	}
	if idx := bytes.IndexByte(name[:], 0); idx >= 0 {
		return string(name[:idx])
	}
	return string(name[:])
}

func findPenDevice() (string, error) {
	data, err := os.ReadFile("/proc/bus/input/devices")
	if err != nil {
		return "", err
	}
	var foundPen bool
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "N: Name=\"") {
			foundPen = strings.Contains(line, "Wacom I2C Digitizer") ||
				strings.Contains(line, "Elan marker input")
		} else if foundPen && strings.HasPrefix(line, "H: Handlers=") {
			if idx := strings.Index(line, "event"); idx >= 0 {
				field := strings.Fields(line[idx:])[0]
				return "/dev/input/" + field, nil
			}
		}
	}
	return "", fmt.Errorf("no pen device found")
}

func cleanup(uifd, evfd int, devPath string) {
	unix.Unmount(devPath, unix.MNT_DETACH)
	ioctl(uifd, UI_DEV_DESTROY, 0)
	unix.Close(uifd)
	ioctl(evfd, EVIOCGRAB, 0)
	unix.Close(evfd)
}

func writeAll(fd int, data []byte) error {
	for len(data) > 0 {
		n, err := unix.Write(fd, data)
		if err != nil {
			return err
		}
		data = data[n:]
	}
	return nil
}

func main() {
	var (
		devPath       string
		configPath    string
		mappings      []string
		pressureGamma float64
		offsetX       int
		offsetY       int
		showVersion   bool
	)

	flag.StringVarP(&devPath, "device", "d", "", "input device path (auto-detected if omitted)")
	flag.StringVar(&configPath, "config", defaultConfigPath, "config file path")
	flag.StringArrayVarP(&mappings, "map", "m", nil, "key mapping FROM=TO (e.g. TOOL_RUBBER=STYLUS)")
	flag.Float64Var(&pressureGamma, "pressure-gamma", 1.0, "pressure curve gamma (>1 softens, <1 amplifies)")
	flag.IntVar(&offsetX, "offset-x", 0, "X axis offset (positive = right, negative = left)")
	flag.IntVar(&offsetY, "offset-y", 0, "Y axis offset (positive = down, negative = up)")
	flag.BoolVarP(&showVersion, "version", "v", false, "print version")
	flag.BoolVarP(&verbose, "verbose", "V", false, "verbose logging")
	flag.Parse()

	if showVersion {
		fmt.Println("stylus-remapper", version)
		return
	}

	cfg, err := loadConfig(configPath)
	if err == nil {
		logv("loaded config: %s", configPath)
		if !flag.CommandLine.Changed("pressure-gamma") && cfg.PressureGamma != 0 {
			pressureGamma = cfg.PressureGamma
		}
		if !flag.CommandLine.Changed("offset-x") {
			offsetX = cfg.OffsetX
		}
		if !flag.CommandLine.Changed("offset-y") {
			offsetY = cfg.OffsetY
		}
		if !flag.CommandLine.Changed("map") && len(cfg.Mappings) > 0 {
			mappings = cfg.Mappings
		}
	} else if !os.IsNotExist(err) {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}


	if pressureGamma <= 0 {
		fmt.Fprintln(os.Stderr, "pressure-gamma must be positive")
		os.Exit(1)
	}

	if devPath == "" {
		detected, err := findPenDevice()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		devPath = detected
		logv("detected pen device: %s", devPath)
	}

	for _, m := range mappings {
		if _, _, err := parseMapping(m); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}

	evfd, err := openRealDevice(devPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "open input:", err)
		os.Exit(1)
	}
	logv("opened real device: %s", devPath)

	uifd, err := unix.Open("/dev/uinput", unix.O_WRONLY|unix.O_NONBLOCK, 0)
	if err != nil {
		unix.Close(evfd)
		fmt.Fprintln(os.Stderr, "open uinput:", err)
		os.Exit(1)
	}

	ioctlInt(uifd, UI_SET_EVBIT, EV_SYN)
	ioctlInt(uifd, UI_SET_EVBIT, EV_KEY)
	ioctlInt(uifd, UI_SET_EVBIT, EV_ABS)

	allKeys := make(map[uint16]bool)
	for _, code := range keyNames {
		allKeys[code] = true
	}
	for code := range allKeys {
		ioctlInt(uifd, UI_SET_KEYBIT, int(code))
	}

	absAxes := []uint16{ABS_X, ABS_Y, ABS_PRESSURE, ABS_DISTANCE, ABS_TILT_X, ABS_TILT_Y}
	absRange := make(map[uint16]absInfo)
	for _, axis := range absAxes {
		ioctlInt(uifd, UI_SET_ABSBIT, int(axis))
		ai, err := getAbsInfo(evfd, axis)
		if err != nil {
			continue
		}
		absRange[axis] = ai
		setupUinputAbs(uifd, axis, ai)
	}
	pressureMax := absRange[ABS_PRESSURE].Maximum

	initialCfg := config{
		PressureGamma: pressureGamma,
		OffsetX:       offsetX,
		OffsetY:       offsetY,
		Mappings:      mappings,
	}
	rc, err := buildRuntimeConfig(initialCfg, pressureMax)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	var rtCfg atomic.Pointer[runtimeConfig]
	rtCfg.Store(rc)
	logv("pressure gamma: %.2f (max %d)", pressureGamma, pressureMax)
	logv("offset x: %d, y: %d", offsetX, offsetY)
	logv("mappings: %v", mappings)

	go watchConfig(configPath, pressureMax, &rtCfg)

	devName := getDeviceName(evfd)
	devID := getDeviceID(evfd)
	logv("device name: %s, id: %v", devName, devID)
	var setup uinputSetup
	copy(setup.Name[:], devName)
	setup.ID = devID
	ioctl(uifd, UI_DEV_SETUP, uintptr(unsafe.Pointer(&setup)))

	if err := ioctl(uifd, UI_DEV_CREATE, 0); err != nil {
		fmt.Fprintln(os.Stderr, "uinput create:", err)
		os.Exit(1)
	}

	var uinputEventPath string
	for range 50 {
		uinputEventPath, err = findUinputEventPath(uifd)
		if err == nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "find uinput event:", err)
		ioctl(uifd, UI_DEV_DESTROY, 0)
		unix.Close(uifd)
		unix.Close(evfd)
		os.Exit(1)
	}

	if err := ioctl(evfd, EVIOCGRAB, 1); err != nil {
		fmt.Fprintln(os.Stderr, "grab:", err)
		ioctl(uifd, UI_DEV_DESTROY, 0)
		unix.Close(uifd)
		unix.Close(evfd)
		os.Exit(1)
	}

	if err := unix.Mount(uinputEventPath, devPath, "", unix.MS_BIND, ""); err != nil {
		fmt.Fprintf(os.Stderr, "bind mount %s -> %s: %v\n", uinputEventPath, devPath, err)
		ioctl(evfd, EVIOCGRAB, 0)
		ioctl(uifd, UI_DEV_DESTROY, 0)
		unix.Close(uifd)
		unix.Close(evfd)
		os.Exit(1)
	}
	logv("bind mounted %s over %s", uinputEventPath, devPath)
	logv("remapping active")

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, unix.SIGINT, unix.SIGTERM)
	go func() {
		<-sigCh
		cleanup(uifd, evfd, devPath)
		os.Exit(0)
	}()


	evSize := int(unsafe.Sizeof(inputEventRaw{}))
	buf := make([]byte, evSize)
	for {
		_, err := unix.Read(evfd, buf)
		if err != nil {
			fmt.Fprintln(os.Stderr, "read:", err)
			break
		}

		ev := (*inputEventRaw)(unsafe.Pointer(&buf[0]))
		cfg := rtCfg.Load()

		if ev.Type == EV_KEY {
			if to, ok := cfg.remapTable[ev.Code]; ok {
				ev.Code = to
			}
		}

		if ev.Type == EV_ABS && ev.Code == ABS_PRESSURE && cfg.pressureLUT != nil &&
			ev.Val >= 0 && ev.Val < int32(len(cfg.pressureLUT)) {
			ev.Val = cfg.pressureLUT[ev.Val]
		}

		if ev.Type == EV_ABS && (ev.Code == ABS_X || ev.Code == ABS_Y) {
			offset := cfg.offsetX
			if ev.Code == ABS_Y {
				offset = cfg.offsetY
			}
			if offset != 0 {
				ai := absRange[ev.Code]
				v := ev.Val + offset
				if v < ai.Minimum {
					v = ai.Minimum
				} else if v > ai.Maximum {
					v = ai.Maximum
				}
				ev.Val = v
			}
		}

		out := unsafe.Slice((*byte)(unsafe.Pointer(ev)), evSize)
		if err := writeAll(uifd, out); err != nil {
			fmt.Fprintln(os.Stderr, "write:", err)
			break
		}
	}

	cleanup(uifd, evfd, devPath)
}
