# 附录：源码文件分配索引

下面列出了 Go 源码文件所实现的功能，以及他们主要功能的介绍，在本书正文中对应的（粗略）位置：

```
    ├── cmd
    │   └── cgo                            10-cgo.md
    ├── net
    ├── reflect
    ├── runtime
    │   ├── README.md

    // boot                                1-init.md
    │   ├── rt0_darwin_amd64.s
    │   ├── rt0_js_wasm.s
    │   ├── rt0_linux_amd64.s
    │   ├── runtime1.go
    │   ├── os_darwin.go                8-runtime/syscall-darwin.md
    │   ├── os_linux.go                 8-runtime/syscall-linux.md
    │   ├── go_tls.h

    // sched                            4-sched/*.md
    │   ├── runtime.go
    │   ├── runtime2.go
    │   ├── proc.go
    │   ├── stack.go

    // mem                                5-mem/*.md
    │   ├── malloc.go
    │   ├── mcache.go
    │   ├── mcentral.go
    │   ├── mprof.go
    │   ├── mfixalloc.go
    │   ├── mheap.go
    │   ├── mmap.go
    │   ├── msize.go
    │   ├── mstats.go
    │   ├── mkduff.go
    │   ├── duff_amd64.s
    │   ├── mksizeclasses.go
    │   ├── sizeclasses.go
    │   ├── mem_darwin.go
    │   ├── mem_js.go
    │   ├── mem_linux.go
    │   ├── memclr_amd64.s
    │   ├── memclr_wasm.s
    │   ├── memmove_amd64.s
    │   ├── memmove_wasm.s
    │   ├── msan.go
    │   ├── msan0.go
    │   ├── msan_amd64.s

    // GC                                 6-GC/*.md
    │   ├── mgc.go
    │   ├── mgclarge.go
    │   ├── mgcmark.go
    │   ├── mgcsweep.go
    │   ├── mgcsweepbuf.go
    │   ├── mgcwork.go
    │   ├── mbarrier.go
    │   ├── mwbbuf.go
    │   ├── mbitmap.go
    │   ├── lfstack.go
    │   ├── lfstack_64bit.go

    // common
    │   ├── asm.s
    │   ├── asm_amd64.s
    │   ├── asm_wasm.s
    │   ├── atomic_pointer.go

    // types/keywords
    │   ├── type.go
    │   ├── typekind.go
    //   float
    │   ├── softfloat64.go
    │   ├── float.go
    //   map
    │   ├── fastlog2.go
    │   ├── mkfastlog2table.go
    │   ├── fastlog2table.go
    │   ├── alg.go
    │   ├── map.go
    │   ├── map_fast32.go
    │   ├── map_fast64.go
    │   ├── map_faststr.go
    │   ├── hash64.go
    │   ├── heapdump.go
    //   interface
    │   ├── iface.go
    //   chan/select
    │   ├── chan.go
    │   ├── select.go
    //   slice
    │   ├── slice.go
    //   string
    │   ├── string.go
    │   ├── utf8.go
    //   panic
    │   ├── panic.go

    // locks
    │   ├── sema.go
    │   ├── lock_futex.go
    │   ├── lock_js.go
    │   ├── lock_sema.go
    │   ├── rwmutex.go

    // net
    │   ├── netpoll.go
    │   ├── netpoll_epoll.go
    │   ├── netpoll_fake.go
    │   ├── netpoll_kqueue.go
    │   ├── netpoll_stub.go

    // cgo                                     10-cgo.md
    │   ├── cgo                    
    │   ├── cgo.go
    │   ├── cgo_mmap.go
    │   ├── cgo_sigaction.go
    │   ├── cgocall.go
    │   ├── cgocallback.go
    │   ├── cgocheck.go
    │   ├── cpuprof.go
    │   ├── textflag.h
    │   ├── funcdata.h
    │   ├── defs_linux_amd64.go
    │   ├── defs_darwin_amd64.go
    │   ├── plugin.go

    // extern
    │   ├── extern.go
    │   ├── symtab.go
    │   ├── mfinal.go              8-runtime/finalizer.md

    // signal
    │   ├── os_linux_generic.go
    │   ├── sigaction.go
    │   ├── signal_amd64x.go
    │   ├── signal_darwin.go
    │   ├── signal_darwin_amd64.go
    │   ├── signal_linux_amd64.go
    │   ├── signal_unix.go
    │   ├── sigqueue.go
    │   ├── sigtab_linux_generic.go

    // time
    │   ├── time.go
    │   ├── timestub.go
    │   ├── timestub2.go

    // race/trace/pprof
    │   ├── profbuf.go
    │   ├── proflabel.go
    |   ├── race.go
    |   ├── race0.go
    |   ├── race_amd64.s
    |   ├── trace.go
    |   ├── traceback.go
    │   ├── debug.go
    │   ├── debugcall.go

    // call utils
    │   ├── cpuflags_amd64.go
    │   ├── cputicks.go
    │   ├── env_posix.go
    │   ├── error.go
    │   ├── print.go
    │   ├── write_err.go
    │   ├── relax_stub.go
    │   ├── stubs.go
    │   ├── stubs2.go
    │   ├── stubs3.go
    │   ├── stubs_linux.go
    │   ├── stubs_nonlinux.go
    │   ├── stubs_x86.go
    │   ├── sys_darwin.go
    │   ├── sys_darwin_amd64.s
    │   ├── sys_linux_amd64.s
    │   ├── sys_wasm.go
    │   ├── sys_wasm.s
    │   ├── sys_x86.go
    │   ├── unaligned1.go
    │   ├── vdso_elf64.go
    │   ├── vdso_linux_amd64.go

    │   └── internal
    │       ├── atomic              11-pkg/sync/atomic.md
    │       └── sys                 11-pkg/syscall/syscall.md
    ├── sync
    │   ├── atomic                11-pkg/atomic/atomic.md
    │   ├── cond.go               11-pkg/sync/cond.md
    │   ├── map.go                11-pkg/sync/map.md
    │   ├── mutex.go              11-pkg/sync/mutex.md
    │   ├── once.go               11-pkg/sync/once.md
    │   ├── pool.go               11-pkg/sync/pool.md
    │   ├── runtime.go
    │   ├── rwmutex.go            11-pkg/sync/mutex.md
    │   └── waitgroup.go          11-pkg/sync/waitgroup.md
    │
    ├── syscall
    └── unsafe                      9-unsafe.md
```

## 许可

[Go under the hood](https://github.com/changkun/go-under-the-hood) | CC-BY-NC-ND 4.0 & MIT &copy; [changkun](https://changkun.de)
