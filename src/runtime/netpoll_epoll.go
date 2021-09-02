// Copyright 2013 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// +build linux

package runtime

import (
	"runtime/internal/atomic"
	"unsafe"
)

func epollcreate(size int32) int32
func epollcreate1(flags int32) int32

//go:noescape
func epollctl(epfd, op, fd int32, ev *epollevent) int32

//go:noescape
func epollwait(epfd int32, ev *epollevent, nev, timeout int32) int32
func closeonexec(fd int32)

var (
	// 全局唯一的 epoll fd，只在 listener fd 初始化之时被指定一次
	epfd int32 = -1 // epoll descriptor

	netpollBreakRd, netpollBreakWr uintptr // for netpollBreak

	netpollWakeSig uint32 // used to avoid duplicate calls of netpollBreak
)

// 初始化网络轮询器，通过 sync.Once 和 netpollInited 变量保证函数只会调用一次
// netpollinit 会创建一个 epoll 实例，然后把 epoll fd 赋值给 epfd，
// 后续 listener 以及它 accept 的所有 sockets 有关 epoll 的操作都是基于这个全局的 epfd

// 创建实例，等于: int epoll_create(int size);
func netpollinit() {
	// 调用 epollcreate1 创建一个 epoll 实例 epfd，作为整个 runtime 的唯一 event-loop 使用
	epfd = epollcreate1(_EPOLL_CLOEXEC)
	if epfd < 0 {
		epfd = epollcreate(1024)
		if epfd < 0 {
			println("runtime: epollcreate failed with", -epfd)
			throw("runtime: netpollinit failed")
		}
		closeonexec(epfd)
	}
	// 调用 runtime.nonblockingPipe 创建一个用于和 epoll 实例通信的管道，这里为什么不用更新且更轻量的 eventfd 呢？我个人猜测是为了兼容更多以及更老的系统版本
	r, w, errno := nonblockingPipe()
	if errno != 0 {
		println("runtime: pipe failed with", -errno)
		throw("runtime: pipe failed")
	}
	// 将 netpollBreakRd 通知信号量封装成 epollevent 事件结构体注册进 epoll 实例
	ev := epollevent{
		events: _EPOLLIN,
	}
	*(**uintptr)(unsafe.Pointer(&ev.data)) = &netpollBreakRd
	// 将用于读取数据的文件描述符打包成 epollevent 事件加入监听
	errno = epollctl(epfd, _EPOLL_CTL_ADD, r, &ev)
	if errno != 0 {
		println("runtime: epollctl failed with", -errno)
		throw("runtime: epollctl failed")
	}
	netpollBreakRd = uintptr(r)
	netpollBreakWr = uintptr(w)
}

// 判断文件描述符是否被轮询器使用
func netpollIsPollDescriptor(fd uintptr) bool {
	return fd == uintptr(epfd) || fd == netpollBreakRd || fd == netpollBreakWr
}

// 监听文件描述符上的边缘触发事件，创建事件并加入监听
// netpollopen 会被 runtime_pollOpen 调用（poll_runtime_pollOpen），注册 fd 到 epoll 实例，
// 注意这里使用的是 epoll 的 ET 模式，同时会利用万能指针把 pollDesc 保存到 epollevent 的一个 8 位的字节数组 data 里

// 注册 fd，等于: int epoll_ctl(int epfd, int op, int fd, struct epoll_event *event);
func netpollopen(fd uintptr, pd *pollDesc) int32 {
	var ev epollevent
	ev.events = _EPOLLIN | _EPOLLOUT | _EPOLLRDHUP | _EPOLLET
	*(**pollDesc)(unsafe.Pointer(&ev.data)) = pd
	return -epollctl(epfd, _EPOLL_CTL_ADD, int32(fd), &ev)
}

func netpollclose(fd uintptr) int32 {
	var ev epollevent
	return -epollctl(epfd, _EPOLL_CTL_DEL, int32(fd), &ev)
}

func netpollarm(pd *pollDesc, mode int) {
	throw("runtime: unused")
}

// netpollBreak interrupts an epollwait.
// 唤醒网络轮询器，例如：计时器向前修改时间时会通过该函数中断网络轮询器
// netpollBreak 往通信管道里写入信号去唤醒 epollwait
func netpollBreak() {
	// 通过 CAS 避免重复的唤醒信号被写入管道，从而减少系统调用并节省一些系统资源

	if atomic.Cas(&netpollWakeSig, 0, 1) {
		for {
			var b byte
			n := write(netpollBreakWr, unsafe.Pointer(&b), 1)
			if n == 1 {
				break
			}
			if n == -_EINTR {
				continue
			}
			if n == -_EAGAIN {
				return
			}
			println("runtime: netpollBreak write failed with", -n)
			throw("runtime: netpollBreak write failed")
		}
	}
}

// netpoll checks for ready network connections.
// Returns list of goroutines that become runnable.
// delay < 0: blocks indefinitely
// delay == 0: does not block, just polls
// delay > 0: block for up to that many nanoseconds
// 轮询网络并返回一组已经准备就绪的 Goroutine，传入的参数会决定它的行为；
// 如果参数小于 0，无限期等待文件描述符就绪；
// 如果参数等于 0，非阻塞地轮询网络；
// 如果参数大于 0，阻塞特定时间轮询网络；

// 等待事件，等于: int epoll_wait(int epfd, struct epoll_event * events, int maxevents, int timeout);
func netpoll(delay int64) gList {
	if epfd == -1 {
		return gList{}
	}
	var waitms int32
	// 根据调用方的入参 delay，设置对应的调用 epollwait 的 timeout 值；
	if delay < 0 {
		waitms = -1
	} else if delay == 0 {
		waitms = 0
	} else if delay < 1e6 {
		waitms = 1
	} else if delay < 1e15 {
		waitms = int32(delay / 1e6)
	} else {
		// An arbitrary cap on how long to wait for a timer.
		// 1e9 ms == ~11.5 days.
		waitms = 1e9
	}
	var events [128]epollevent
retry:
	// 调用 epollwait 等待发生了可读/可写事件的 fd；
	// 循环 epollwait 返回的事件列表，处理对应的事件类型，组装可运行的 goroutine 链表并返回。
	n := epollwait(epfd, &events[0], int32(len(events)), waitms)
	if n < 0 {
		if n != -_EINTR {
			println("runtime: epollwait on fd", epfd, "failed with", -n)
			throw("runtime: netpoll failed")
		}
		// If a timed sleep was interrupted, just return to
		// recalculate how long we should sleep now.
		if waitms > 0 {
			return gList{}
		}
		goto retry
	}
	// toRun 是一个 g 的链表，存储要恢复的 goroutines，最后返回给调用方
	var toRun gList
	for i := int32(0); i < n; i++ {
		ev := &events[i]
		if ev.events == 0 {
			continue
		}

		// Go scheduler 在调用 findrunnable() 寻找 goroutine 去执行的时候，
		// 在调用 netpoll 之时会检查当前是否有其他线程同步阻塞在 netpoll，
		// 若是，则调用 netpollBreak 来唤醒那个线程，避免它长时间阻塞
		if *(**uintptr)(unsafe.Pointer(&ev.data)) == &netpollBreakRd {
			if ev.events != _EPOLLIN {
				println("runtime: netpoll: break fd ready for", ev.events)
				throw("runtime: netpoll: break fd ready for something unexpected")
			}
			if delay != 0 {
				// netpollBreak could be picked up by a
				// nonblocking poll. Only read the byte
				// if blocking.
				var tmp [16]byte
				read(int32(netpollBreakRd), noescape(unsafe.Pointer(&tmp[0])), int32(len(tmp)))
				atomic.Store(&netpollWakeSig, 0)
			}
			continue
		}

		// 判断发生的事件类型，读类型或者写类型等，然后给 mode 复制相应的值，
		// mode 用来决定从 pollDesc 里的 rg 还是 wg 里取出 goroutine
		var mode int32
		if ev.events&(_EPOLLIN|_EPOLLRDHUP|_EPOLLHUP|_EPOLLERR) != 0 {
			mode += 'r'
		}
		if ev.events&(_EPOLLOUT|_EPOLLHUP|_EPOLLERR) != 0 {
			mode += 'w'
		}
		if mode != 0 {
			// 取出保存在 epollevent 里的 pollDesc
			pd := *(**pollDesc)(unsafe.Pointer(&ev.data))
			pd.everr = false
			if ev.events == _EPOLLERR {
				pd.everr = true
			}
			// 调用 netpollready，传入就绪 fd 的 pollDesc，
			// 把 fd 对应的 goroutine 添加到链表 toRun 中
			netpollready(&toRun, pd, mode)
		}
	}
	return toRun
}
