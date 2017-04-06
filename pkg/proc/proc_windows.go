package proc

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"unsafe"

	sys "golang.org/x/sys/windows"
)

// OSProcessDetails holds Windows specific information.
type OSProcessDetails struct {
	hProcess    syscall.Handle
	breakThread int
}

// Launch creates and begins debugging a new process.
func Launch(cmd []string, wd string) (*Process, error) {
	argv0Go, err := filepath.Abs(cmd[0])
	if err != nil {
		return nil, err
	}

	// Make sure the binary exists and is an executable file
	if filepath.Base(cmd[0]) == cmd[0] {
		if _, err := exec.LookPath(cmd[0]); err != nil {
			return nil, err
		}
	}

	_, closer, err := openExecutablePathPE(argv0Go)
	if err != nil {
		return nil, NotExecutableErr
	}
	closer.Close()

	// Duplicate the stdin/stdout/stderr handles
	files := []uintptr{uintptr(syscall.Stdin), uintptr(syscall.Stdout), uintptr(syscall.Stderr)}
	p, _ := syscall.GetCurrentProcess()
	fd := make([]syscall.Handle, len(files))
	for i := range files {
		err := syscall.DuplicateHandle(p, syscall.Handle(files[i]), p, &fd[i], 0, true, syscall.DUPLICATE_SAME_ACCESS)
		if err != nil {
			return nil, err
		}
		defer syscall.CloseHandle(syscall.Handle(fd[i]))
	}

	argv0, err := syscall.UTF16PtrFromString(argv0Go)
	if err != nil {
		return nil, err
	}

	// create suitable command line for CreateProcess
	// see https://github.com/golang/go/blob/master/src/syscall/exec_windows.go#L326
	// adapted from standard library makeCmdLine
	// see https://github.com/golang/go/blob/master/src/syscall/exec_windows.go#L86
	var cmdLineGo string
	if len(cmd) >= 1 {
		for _, v := range cmd {
			if cmdLineGo != "" {
				cmdLineGo += " "
			}
			cmdLineGo += syscall.EscapeArg(v)
		}
	}

	var cmdLine *uint16
	if cmdLineGo != "" {
		if cmdLine, err = syscall.UTF16PtrFromString(cmdLineGo); err != nil {
			return nil, err
		}
	}

	var workingDir *uint16
	if wd != "" {
		if workingDir, err = syscall.UTF16PtrFromString(wd); err != nil {
			return nil, err
		}
	}

	// Initialize the startup info and create process
	si := new(sys.StartupInfo)
	si.Cb = uint32(unsafe.Sizeof(*si))
	si.Flags = syscall.STARTF_USESTDHANDLES
	si.StdInput = sys.Handle(fd[0])
	si.StdOutput = sys.Handle(fd[1])
	si.StdErr = sys.Handle(fd[2])
	pi := new(sys.ProcessInformation)

	dbp := New(0)
	dbp.execPtraceFunc(func() {
		if wd == "" {
			err = sys.CreateProcess(argv0, cmdLine, nil, nil, true, _DEBUG_ONLY_THIS_PROCESS, nil, nil, si, pi)
		} else {
			err = sys.CreateProcess(argv0, cmdLine, nil, nil, true, _DEBUG_ONLY_THIS_PROCESS, nil, workingDir, si, pi)
		}
	})
	if err != nil {
		return nil, err
	}
	sys.CloseHandle(sys.Handle(pi.Process))
	sys.CloseHandle(sys.Handle(pi.Thread))

	dbp.pid = int(pi.ProcessId)

	return newDebugProcess(dbp, argv0Go)
}

// newDebugProcess prepares process pid for debugging.
func newDebugProcess(dbp *Process, exepath string) (*Process, error) {
	// It should not actually be possible for the
	// call to waitForDebugEvent to fail, since Windows
	// will always fire a CREATE_PROCESS_DEBUG_EVENT event
	// immediately after launching under DEBUG_ONLY_THIS_PROCESS.
	// Attaching with DebugActiveProcess has similar effect.
	var err error
	var tid, exitCode int
	dbp.execPtraceFunc(func() {
		tid, exitCode, err = dbp.waitForDebugEvent(waitBlocking)
	})
	if err != nil {
		return nil, err
	}
	if tid == 0 {
		dbp.postExit()
		return nil, ProcessExitedError{Pid: dbp.pid, Status: exitCode}
	}
	// Suspend all threads so that the call to _ContinueDebugEvent will
	// not resume the target.
	for _, thread := range dbp.threads {
		_, err := _SuspendThread(thread.os.hThread)
		if err != nil {
			return nil, err
		}
	}

	dbp.execPtraceFunc(func() {
		err = _ContinueDebugEvent(uint32(dbp.pid), uint32(dbp.os.breakThread), _DBG_CONTINUE)
	})
	if err != nil {
		return nil, err
	}

	return initializeDebugProcess(dbp, exepath, false)
}

// findExePath searches for process pid, and returns its executable path.
func findExePath(pid int) (string, error) {
	// Original code suggested different approach (see below).
	// Maybe it could be useful in the future.
	//
	// Find executable path from PID/handle on Windows:
	// https://msdn.microsoft.com/en-us/library/aa366789(VS.85).aspx

	p, err := syscall.OpenProcess(syscall.PROCESS_QUERY_INFORMATION, false, uint32(pid))
	if err != nil {
		return "", err
	}
	defer syscall.CloseHandle(p)

	n := uint32(128)
	for {
		buf := make([]uint16, int(n))
		err = _QueryFullProcessImageName(p, 0, &buf[0], &n)
		switch err {
		case syscall.ERROR_INSUFFICIENT_BUFFER:
			// try bigger buffer
			n *= 2
			// but stop if it gets too big
			if n > 10000 {
				return "", err
			}
		case nil:
			return syscall.UTF16ToString(buf[:n]), nil
		default:
			return "", err
		}
	}
}

// Attach to an existing process with the given PID.
func Attach(pid int) (*Process, error) {
	// TODO: Probably should have SeDebugPrivilege before starting here.
	err := _DebugActiveProcess(uint32(pid))
	if err != nil {
		return nil, err
	}
	exepath, err := findExePath(pid)
	if err != nil {
		return nil, err
	}
	return newDebugProcess(New(pid), exepath)
}

// Kill kills the process.
func (dbp *Process) Kill() error {
	if dbp.exited {
		return nil
	}
	if !dbp.threads[dbp.pid].Stopped() {
		return errors.New("process must be stopped in order to kill it")
	}
	// TODO: Should not have to ignore failures here,
	// but some tests appear to Kill twice causing
	// this to fail on second attempt.
	_ = syscall.TerminateProcess(dbp.os.hProcess, 1)
	dbp.exited = true
	return nil
}

func (dbp *Process) requestManualStop() error {
	return _DebugBreakProcess(dbp.os.hProcess)
}

func (dbp *Process) updateThreadList() error {
	// We ignore this request since threads are being
	// tracked as they are created/killed in waitForDebugEvent.
	return nil
}

func (dbp *Process) addThread(hThread syscall.Handle, threadID int, attach, suspendNewThreads bool) (*Thread, error) {
	if thread, ok := dbp.threads[threadID]; ok {
		return thread, nil
	}
	thread := &Thread{
		ID:  threadID,
		dbp: dbp,
		os:  new(OSSpecificDetails),
	}
	thread.os.hThread = hThread
	dbp.threads[threadID] = thread
	if dbp.currentThread == nil {
		dbp.SwitchThread(thread.ID)
	}
	if suspendNewThreads {
		_, err := _SuspendThread(thread.os.hThread)
		if err != nil {
			return nil, err
		}
	}
	return thread, nil
}

func findExecutable(path string, pid int) string {
	return path
}

type waitForDebugEventFlags int

const (
	waitBlocking waitForDebugEventFlags = 1 << iota
	waitSuspendNewThreads
)

func (dbp *Process) waitForDebugEvent(flags waitForDebugEventFlags) (threadID, exitCode int, err error) {
	var debugEvent _DEBUG_EVENT
	shouldExit := false
	for {
		continueStatus := uint32(_DBG_CONTINUE)
		var milliseconds uint32 = 0
		if flags&waitBlocking != 0 {
			milliseconds = syscall.INFINITE
		}
		// Wait for a debug event...
		err := _WaitForDebugEvent(&debugEvent, milliseconds)
		if err != nil {
			return 0, 0, err
		}

		// ... handle each event kind ...
		unionPtr := unsafe.Pointer(&debugEvent.U[0])
		switch debugEvent.DebugEventCode {
		case _CREATE_PROCESS_DEBUG_EVENT:
			debugInfo := (*_CREATE_PROCESS_DEBUG_INFO)(unionPtr)
			hFile := debugInfo.File
			if hFile != 0 && hFile != syscall.InvalidHandle {
				err = syscall.CloseHandle(hFile)
				if err != nil {
					return 0, 0, err
				}
			}
			dbp.os.hProcess = debugInfo.Process
			_, err = dbp.addThread(debugInfo.Thread, int(debugEvent.ThreadId), false, flags&waitSuspendNewThreads != 0)
			if err != nil {
				return 0, 0, err
			}
			break
		case _CREATE_THREAD_DEBUG_EVENT:
			debugInfo := (*_CREATE_THREAD_DEBUG_INFO)(unionPtr)
			_, err = dbp.addThread(debugInfo.Thread, int(debugEvent.ThreadId), false, flags&waitSuspendNewThreads != 0)
			if err != nil {
				return 0, 0, err
			}
			break
		case _EXIT_THREAD_DEBUG_EVENT:
			delete(dbp.threads, int(debugEvent.ThreadId))
			break
		case _OUTPUT_DEBUG_STRING_EVENT:
			//TODO: Handle debug output strings
			break
		case _LOAD_DLL_DEBUG_EVENT:
			debugInfo := (*_LOAD_DLL_DEBUG_INFO)(unionPtr)
			hFile := debugInfo.File
			if hFile != 0 && hFile != syscall.InvalidHandle {
				err = syscall.CloseHandle(hFile)
				if err != nil {
					return 0, 0, err
				}
			}
			break
		case _UNLOAD_DLL_DEBUG_EVENT:
			break
		case _RIP_EVENT:
			break
		case _EXCEPTION_DEBUG_EVENT:
			exception := (*_EXCEPTION_DEBUG_INFO)(unionPtr)
			tid := int(debugEvent.ThreadId)

			switch code := exception.ExceptionRecord.ExceptionCode; code {
			case _EXCEPTION_BREAKPOINT:

				// check if the exception address really is a breakpoint instruction, if
				// it isn't we already removed that breakpoint and we can't deal with
				// this exception anymore.
				atbp := true
				if thread, found := dbp.threads[tid]; found {
					if data, err := thread.readMemory(exception.ExceptionRecord.ExceptionAddress, dbp.bi.arch.BreakpointSize()); err == nil {
						instr := dbp.bi.arch.BreakpointInstruction()
						for i := range instr {
							if data[i] != instr[i] {
								atbp = false
								break
							}
						}
					}
					if !atbp {
						thread.SetPC(uint64(exception.ExceptionRecord.ExceptionAddress))
					}
				}

				if atbp {
					dbp.os.breakThread = tid
					return tid, 0, nil
				} else {
					continueStatus = _DBG_CONTINUE
				}
			case _EXCEPTION_SINGLE_STEP:
				dbp.os.breakThread = tid
				return tid, 0, nil
			default:
				continueStatus = _DBG_EXCEPTION_NOT_HANDLED
			}
		case _EXIT_PROCESS_DEBUG_EVENT:
			debugInfo := (*_EXIT_PROCESS_DEBUG_INFO)(unionPtr)
			exitCode = int(debugInfo.ExitCode)
			shouldExit = true
		default:
			return 0, 0, fmt.Errorf("unknown debug event code: %d", debugEvent.DebugEventCode)
		}

		// .. and then continue unless we received an event that indicated we should break into debugger.
		err = _ContinueDebugEvent(debugEvent.ProcessId, debugEvent.ThreadId, continueStatus)
		if err != nil {
			return 0, 0, err
		}

		if shouldExit {
			return 0, exitCode, nil
		}
	}
}

func (dbp *Process) trapWait(pid int) (*Thread, error) {
	var err error
	var tid, exitCode int
	dbp.execPtraceFunc(func() {
		tid, exitCode, err = dbp.waitForDebugEvent(waitBlocking)
	})
	if err != nil {
		return nil, err
	}
	if tid == 0 {
		dbp.postExit()
		return nil, ProcessExitedError{Pid: dbp.pid, Status: exitCode}
	}
	th := dbp.threads[tid]
	return th, nil
}

func (dbp *Process) loadProcessInformation(wg *sync.WaitGroup) {
	wg.Done()
}

func (dbp *Process) wait(pid, options int) (int, *sys.WaitStatus, error) {
	return 0, nil, fmt.Errorf("not implemented: wait")
}

func (dbp *Process) setCurrentBreakpoints(trapthread *Thread) error {
	// While the debug event that stopped the target was being propagated
	// other target threads could generate other debug events.
	// After this function we need to know about all the threads
	// stopped on a breakpoint. To do that we first suspend all target
	// threads and then repeatedly call _ContinueDebugEvent followed by
	// waitForDebugEvent in non-blocking mode.
	// We need to explicitly call SuspendThread because otherwise the
	// call to _ContinueDebugEvent will resume execution of some of the
	// target threads.

	err := trapthread.SetCurrentBreakpoint()
	if err != nil {
		return err
	}

	for _, thread := range dbp.threads {
		thread.running = false
		_, err := _SuspendThread(thread.os.hThread)
		if err != nil {
			return err
		}
	}

	for {
		var err error
		var tid int
		dbp.execPtraceFunc(func() {
			err = _ContinueDebugEvent(uint32(dbp.pid), uint32(dbp.os.breakThread), _DBG_CONTINUE)
			if err == nil {
				tid, _, _ = dbp.waitForDebugEvent(waitSuspendNewThreads)
			}
		})
		if err != nil {
			return err
		}
		if tid == 0 {
			break
		}
		err = dbp.threads[tid].SetCurrentBreakpoint()
		if err != nil {
			return err
		}
	}

	return nil
}

func (dbp *Process) exitGuard(err error) error {
	return err
}

func (dbp *Process) resume() error {
	for _, thread := range dbp.threads {
		if thread.CurrentBreakpoint != nil {
			if err := thread.StepInstruction(); err != nil {
				return err
			}
			thread.CurrentBreakpoint = nil
		}
	}

	for _, thread := range dbp.threads {
		thread.running = true
		_, err := _ResumeThread(thread.os.hThread)
		if err != nil {
			return err
		}
	}

	return nil
}

func (dbp *Process) detach(kill bool) error {
	if !kill {
		for _, thread := range dbp.threads {
			_, err := _ResumeThread(thread.os.hThread)
			if err != nil {
				return err
			}
		}
	}
	return PtraceDetach(dbp.pid, 0)
}

func killProcess(pid int) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return p.Kill()
}
