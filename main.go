package main

/*
#include <linux/uinput.h>

int eviocgbit0 = EVIOCGBIT(0, EV_MAX);

int eviocgbit_key = EVIOCGBIT(EV_KEY, KEY_MAX);

*/
import "C"

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"math"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
	"unsafe"

	"github.com/reusee/e/v2"
)

var (
	pt     = fmt.Printf
	me     = e.Default
	ce, he = e.New(me)
)

func main() {

	configFilePath := "/etc/kb.conf"

	var paths []string
	configContent, err := ioutil.ReadFile(configFilePath)
	if os.IsNotExist(err) {
		// init
		paths, err = filepath.Glob("/dev/input/by-id/*")
		ce(err)
		configContent, err = json.Marshal(paths)
		ce(err)
		buf := new(bytes.Buffer)
		ce(json.Indent(buf, configContent, "", "    "))
		configContent = buf.Bytes()
		pt("%s\n", configContent)
		ce(ioutil.WriteFile(configFilePath, configContent, 0644))
		pt("init\n")
		return
	} else {
		ce(json.Unmarshal(configContent, &paths))
	}

	KeyboardPath := paths[0]
	if KeyboardPath == "" {
		panic("no keyboard")
	}
	pt("selected %s\n", KeyboardPath)

	keyboardFD, err := syscall.Open(KeyboardPath, syscall.O_RDONLY, 0644)
	if err != nil {
		panic(err)
	}
	defer syscall.Close(keyboardFD)

	file, err := os.OpenFile(
		"/dev/uinput",
		os.O_WRONLY|syscall.O_NONBLOCK,
		0,
	)
	if err != nil {
		panic(err)
	}
	defer file.Close()

	fd := int(file.Fd())
	ctl(fd, C.UI_SET_EVBIT, C.EV_KEY)
	for key := uintptr(C.KEY_RESERVED); key <= C.KEY_MAX; key++ {
		ctl(fd, C.UI_SET_KEYBIT, key)
	}
	var setup C.struct_uinput_setup
	setup.id.bustype = C.BUS_USB
	setup.id.vendor = 0xdead
	setup.id.product = 0xbeef
	setup.name[0] = 'f'
	setup.name[1] = 'o'
	setup.name[2] = 'o'
	ctl(fd, C.UI_DEV_SETUP, uintptr(unsafe.Pointer(&setup)))
	ctl(fd, C.UI_DEV_CREATE, 0)
	time.Sleep(time.Second)
	defer ctl(fd, C.UI_DEV_DESTROY, 0)

	writeEv := func(raw []byte) {

		// filter
		ev := (*C.struct_input_event)(unsafe.Pointer(&raw[0]))
		if ev.code == C.KEY_CAPSLOCK {
			return
		}

		if _, err := syscall.Write(fd, raw); err != nil {
			panic(err)
		}
	}

	ctl(
		keyboardFD,
		C.EVIOCGRAB,
		1,
	)
	defer func() {
		ctl(
			keyboardFD,
			C.EVIOCGRAB,
			0,
		)
	}()

	tickDuration := time.Millisecond * 3
	timeoutDuration := time.Millisecond * 200
	timeout := int(math.Floor(float64(timeoutDuration) / float64(tickDuration)))

	go func() {
		ctrlPress := rawEvent(C.EV_KEY, C.KEY_LEFTCTRL, 1)
		ctrlRelease := rawEvent(C.EV_KEY, C.KEY_LEFTCTRL, 0)
		metaPress := rawEvent(C.EV_KEY, C.KEY_LEFTMETA, 1)
		metaRelease := rawEvent(C.EV_KEY, C.KEY_LEFTMETA, 0)

		type timeoutFn func() (
			full bool,
		)
		type stateFunc func(
			ev *C.struct_input_event,
			raw []byte,
		) (
			next stateFunc,
			full bool,
			timeout int,
			timeoutFn timeoutFn,
		)

		timeoutNoMatch := timeoutFn(func() bool {
			return false
		})
		timeoutFullMatch := timeoutFn(func() bool {
			return true
		})

		var doubleShiftToCtrl stateFunc
		doubleShiftToCtrl = func(ev *C.struct_input_event, raw []byte) (stateFunc, bool, int, timeoutFn) {
			if ev._type != C.EV_KEY {
				return nil, false, 0, nil
			}
			if ev.value != 1 {
				return nil, false, 0, nil
			}
			if ev.code != C.KEY_LEFTSHIFT && ev.code != C.KEY_RIGHTSHIFT {
				return nil, false, 0, nil
			}
			code := ev.code
			var waitNextShift stateFunc
			waitNextShift = func(ev *C.struct_input_event, raw []byte) (stateFunc, bool, int, timeoutFn) {
				if ev._type != C.EV_KEY {
					return waitNextShift, false, timeout, timeoutNoMatch
				}
				if ev.value != 1 {
					/*
						key press -> shift press -> shift release -> key release
						这个序列会导致 key release 事件被延迟，会产生 key repeat
						所以在 key release 时，中止匹配
					*/
					if ev.code != code {
						return nil, false, 0, nil
					}
					return waitNextShift, false, timeout, timeoutNoMatch
				}
				if ev.code != code {
					return nil, false, 0, nil
				}
				var waitNextKey stateFunc
				waitNextKey = func(ev *C.struct_input_event, raw []byte) (stateFunc, bool, int, timeoutFn) {
					if ev._type != C.EV_KEY {
						return waitNextKey, false, timeout, timeoutNoMatch
					}
					if ev.value != 1 {
						return waitNextKey, false, timeout, timeoutNoMatch
					}
					if ev.code == C.KEY_LEFTSHIFT || ev.code == C.KEY_RIGHTSHIFT {
						return nil, false, 0, nil
					}
					writeEv(ctrlPress)
					writeEv(raw)
					writeEv(ctrlRelease)
					return nil, true, 0, nil
				}
				return waitNextKey, false, timeout, timeoutNoMatch
			}
			return waitNextShift, false, timeout, timeoutNoMatch
		}

		var capslockToMeta stateFunc
		capslockToMeta = func(ev *C.struct_input_event, raw []byte) (stateFunc, bool, int, timeoutFn) {
			if ev._type != C.EV_KEY {
				return nil, false, 0, nil
			}
			if ev.value != 1 {
				return nil, false, 0, nil
			}
			if ev.code != C.KEY_CAPSLOCK {
				return nil, false, 0, nil
			}
			var waitNextKey stateFunc
			waitNextKey = func(ev *C.struct_input_event, raw []byte) (stateFunc, bool, int, timeoutFn) {
				if ev._type != C.EV_KEY {
					return waitNextKey, false, timeout, timeoutFullMatch
				}
				if ev.value != 1 {
					/*
						key press -> capslock press -> capslock release -> key release
						这个序列会导致 key release 事件被延迟，产生 key repeat
						所以在 key release 时，写入该 release 事件，并返回匹配成功
					*/
					if ev.code != C.KEY_CAPSLOCK {
						writeEv(raw)
						return nil, true, 0, nil
					}
					return waitNextKey, false, timeout, timeoutFullMatch
				}
				if ev.code == C.KEY_CAPSLOCK {
					return nil, true, 0, nil
				}
				writeEv(metaPress)
				writeEv(raw)
				writeEv(metaRelease)
				return nil, true, 0, nil
			}
			return waitNextKey, false, timeout, timeoutFullMatch
		}

		fns := []stateFunc{
			doubleShiftToCtrl,
			capslockToMeta,
		}
		type State struct {
			fn        stateFunc
			timeout   int
			timeoutFn timeoutFn
		}
		var states []*State
		var delayed [][]byte

		type Event struct {
			ev  *C.struct_input_event
			raw []byte
		}
		evCh := make(chan Event, 128)

		go func() {
			for {
				raw := make([]byte, unsafe.Sizeof(C.struct_input_event{}))
				if _, err := syscall.Read(keyboardFD, raw); err != nil {
					panic(err)
				}
				ev := (*C.struct_input_event)(unsafe.Pointer(&raw[0]))
				evCh <- Event{
					ev:  ev,
					raw: raw,
				}
			}
		}()

		timer := time.NewTimer(tickDuration)
		timerOK := false

		for {
			select {

			case ev := <-evCh:
				var newStates []*State
				hasPartialMatch := false
				hasFullMatch := false
				for _, fn := range fns {
					next, full, t, timeoutFn := fn(ev.ev, ev.raw)
					if full {
						hasFullMatch = true
					} else if next != nil {
						hasPartialMatch = true
						newStates = append(newStates, &State{
							fn:        next,
							timeout:   t,
							timeoutFn: timeoutFn,
						})
					}
				}
				for _, state := range states {
					next, full, t, timeoutFn := state.fn(ev.ev, ev.raw)
					if full {
						hasFullMatch = true
					} else if next != nil {
						hasPartialMatch = true
						newStates = append(newStates, &State{
							fn:        next,
							timeout:   t,
							timeoutFn: timeoutFn,
						})
					}
				}
				states = newStates
				if hasPartialMatch {
					// delay key
					delayed = append(delayed, ev.raw)
				}
				if hasFullMatch {
					// clear delayed key
					delayed = delayed[0:0:cap(delayed)]
				}
				if !hasPartialMatch && !hasFullMatch {
					// pop keys
					for _, r := range delayed {
						writeEv(r)
					}
					delayed = delayed[0:0:cap(delayed)]
					writeEv(ev.raw)
				}
				if (len(states) > 0 || len(delayed) > 0) && !timerOK {
					if !timer.Stop() {
						select {
						case <-timer.C:
						default:
						}
					}
					timer.Reset(tickDuration)
					timerOK = true
				}

			case <-timer.C:
				hasFullMatch := false
				for i := 0; i < len(states); {
					state := states[i]
					state.timeout--
					if state.timeout == 0 {
						// timeoutFn
						hasFullMatch = hasFullMatch || state.timeoutFn()
						// delete
						copy(
							states[i:len(states)-1],
							states[i+1:len(states)],
						)
						states = states[: len(states)-1 : cap(states)]
						continue
					}
					i++
				}
				if len(states) == 0 {
					if !hasFullMatch {
						// pop
						for _, r := range delayed {
							writeEv(r)
						}
					}
					delayed = delayed[0:0:cap(delayed)]
				}
				timerOK = false

			}
		}

	}()

	sigs := make(chan os.Signal)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGKILL)
	<-sigs
}

func rawEvent(_type C.ushort, code C.ushort, value C.int) []byte {
	raw := make([]byte, unsafe.Sizeof(C.struct_input_event{}))
	ev := (*C.struct_input_event)(unsafe.Pointer(&raw[0]))
	ev._type = _type
	ev.code = code
	ev.value = value
	return raw
}

func ctl(fd int, a1, a2 uintptr) {
	_, _, errno := syscall.Syscall(
		syscall.SYS_IOCTL,
		uintptr(fd),
		a1,
		a2,
	)
	if errno != 0 {
		panic(errno.Error())
	}
}

func ctl2(fd int, a1, a2 uintptr) error {
	_, _, errno := syscall.Syscall(
		syscall.SYS_IOCTL,
		uintptr(fd),
		a1,
		a2,
	)
	if errno != 0 {
		return errno
	}
	return nil
}
