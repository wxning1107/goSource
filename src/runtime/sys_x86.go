// Copyright 2013 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// +build amd64 386

package runtime

import (
	"runtime/internal/sys"
	"unsafe"
)

// adjust Gobuf as if it executed a call to fn with context ctxt
// and then did an immediate gosave.
func gostartcall(buf *gobuf, fn, ctxt unsafe.Pointer) {
	// newg 的栈顶，目前 newg 栈上只有 fn 函数的参数，sp 指向的是 fn 的第一参数
	sp := buf.sp
	if sys.RegSize > sys.PtrSize {
		sp -= sys.PtrSize
		*(*uintptr)(unsafe.Pointer(sp)) = 0
	}
	// 为返回地址预留空间
	sp -= sys.PtrSize
	// 这里填的是 newproc1 函数里设置的 goexit 函数的第二条指令
	// 伪装 fn 是被 goexit 函数调用的，使得 fn 执行完后返回到 goexit 继续执行，从而完成清理工作
	*(*uintptr)(unsafe.Pointer(sp)) = buf.pc
	// 重新设置 buf.sp
	buf.sp = sp
	// 当 goroutine 被调度起来执行时，会从这里的 pc 值开始执行，初始化时就是 runtime.main
	buf.pc = uintptr(fn)
	buf.ctxt = ctxt
}
