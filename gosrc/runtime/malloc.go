// Copyright 2014 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// 内存分配器
//
// 基于 tcmalloc (thread caching malloc)，有些许不同。
// http://goog-perftools.sourceforge.net/doc/tcmalloc.html
//
// 主分配器工作在 page 上。小规模的分配（小于等于 32KB）被舍入为大约 70
// 个大小的 class 中，每个 class 都有一组自己完全相同大小的对象。
// 任何可用的内存 page 可以拆分为一个固定大小等级的集合，
// 然后使用 free bitmap 进行管理。
//
// 分配器的数据结构为:
//
//	fixalloc: 固定大小(fixed-size)非堆(off-heap)中对象的自由表(free-list)分配器
//	mheap: 分配的堆，在 page (8192字节=8KB)的粒度上进行管理。
//	mspan: mheap 管理的一连串 page
//	mcentral: 搜集给定大小的 class 的所有 span
//	mcache: 具有空闲空间的 mspan 的 per-P 缓存
//	mstats: 分配统计
//
// 分配一个小对象会进入缓存层次结构：
//
//	1. 将大小调整为 small 大小等级中的一个，并查看此 P 的 mcache 中相应的 mspan。
//	   扫描 mspan 的 free bitmap 以找到 free slot。如果有 free slot，则分配它。
//	   这可以在不获取锁定的情况下完成。
//
//	2. 如果 mspan 没有 free slot，则从 mcentrals 列表中获取一个
//	   具有所需class大小的空间的新的 mspan。
//	   获取整个 span 均摊了对 mcentral 加锁的成本。
//
//	3. 如果 mcentral 的 mspan 列表为空, 则从 mheap 中获取一连串页给这个 mspan
//
//	4. 如果 mheap 为空，或没有足够的页来分配如此大的对象，则从操作系统中分配一组新的页（至少 1MB）
//	   一次性分配一大片页能够均摊与操作系统沟通的成本
//
// 扫描 mspan 并在其上释放对象会产生类似的层次结构：
//
//	1. 如果 mspan 在响应分配时被扫描，则返回到 mcache 以满足分配需求。
//
//	2. 否则，如果 mspan 仍然在其中分配了对象，
//	   则将其放置在 mspan 的大小等级的 mcentral 自由列表中。
//
//	3. 否则，如果 mspan 中的所有对象都是空闲的，则 mspan 现在处于“空闲”状态，
//	   因此它将返回到 mheap 并且不再具有大小等级。
//	   这可以将其与相邻的空闲 mspans 合并。
//
//	4. 如果 mspan 保持空闲的时间足够长，则将其页面归还给操作系统。
//
// 分配并释放大对象直接使用 mheap，绕过 mcache 和 mcentral。
//
// 仅当 mspan.needzero 为 false 时，mspan 中的自由对象槽才会归零。
// 如果 needzero 为 true，则对象在分配时归零。以这种方式延迟归零有很多好处：
//
//	1. 栈帧分配可以完全避免归零。
//
//	2. 它表现出更好的时间局部性，因为程序可能要写入内存。
//
//	3. 我们不会零页面则永远不会被重用。

// 虚拟内存布局
//
// 堆是 arena 组成的集合，在 64 位机器上为 64MB，在 32 位机器上
// 为 4MB（heapArenaBytes）。每个 arena 的起始地址与 arena 的大小对齐。
//
// 每个 arena 与一个 heapArena 对象关联，用于保存了 arena 的 metadata：
// arena 中所有字的 heap bitmap，和 arena 中所有页的 span map。
// heapArena 对象是在堆外分配的。
//
// 由于 arena 是对齐的，地址空间可以视为一系列 arena 帧。arena map (mheap_.arenas)
// 将 arena 帧数映射为 *heapArena 或 nil （对那些不返回 Go 堆的地址空间）。
// arena map 被构造为两级数组，由 "L1" arena map 和多个 "L2" arena map 组成。
// 然而由于 arena 非常大，在很多体系结构中，arena map 由一个单一的大的 L2 map 组成。
//
// arena map 覆盖了整个可能的地址空间，允许 Go 堆使用地址空间任何部分。
// 分配器会尝试保持 arena 连续，因此大的 span （因此大的对象）可以跨多个 arena。

package runtime

import (
	"runtime/internal/atomic"
	"runtime/internal/math"
	"runtime/internal/sys"
	"unsafe"
)

const (
	debugMalloc = false

	maxTinySize   = _TinySize
	tinySizeClass = _TinySizeClass
	maxSmallSize  = _MaxSmallSize

	pageShift = _PageShift
	pageSize  = _PageSize
	pageMask  = _PageMask
	// By construction, single page spans of the smallest object class
	// have the most objects per span.
	maxObjsPerSpan = pageSize / 8

	concurrentSweep = _ConcurrentSweep

	_PageSize = 1 << _PageShift // 8KB
	_PageMask = _PageSize - 1

	// _64bit = 1 on 64-bit systems, 0 on 32-bit systems
	_64bit = 1 << (^uintptr(0) >> 63) / 2

	// Tiny allocator 参数, see "Tiny allocator" comment in malloc.go.
	_TinySize      = 16
	_TinySizeClass = int8(2)

	_FixAllocChunk = 16 << 10 // FixAlloc 一个 Chunk 的大小

	// Per-P, 每个 order 对应栈所分割的缓存大小
	_StackCacheSize = 32 * 1024

	// 获得缓存的 order 数。order 0 为 FixedStack，每个后序都是前一个的两倍
	// 我们需要缓存 2KB, 4KB, 8KB 和 16KB 的栈，更大的栈则会直接分配.
	// 由于 FixedStack 与操作系统相关，必须动态的计算 NumStackOrders 来保证相同的最大缓存大小
	//   OS               | FixedStack | NumStackOrders
	//   -----------------+------------+---------------
	//   linux/darwin/bsd | 2KB        | 4
	//   windows/32       | 4KB        | 3
	//   windows/64       | 8KB        | 2
	//   plan9            | 4KB        | 3
	_NumStackOrders = 4 - sys.PtrSize/4*sys.GoosWindows - 1*sys.GoosPlan9

	// heapAddrBits is the number of bits in a heap address. On
	// amd64, addresses are sign-extended beyond heapAddrBits. On
	// other arches, they are zero-extended.
	//
	// On most 64-bit platforms, we limit this to 48 bits based on a
	// combination of hardware and OS limitations.
	//
	// amd64 hardware limits addresses to 48 bits, sign-extended
	// to 64 bits. Addresses where the top 16 bits are not either
	// all 0 or all 1 are "non-canonical" and invalid. Because of
	// these "negative" addresses, we offset addresses by 1<<47
	// (arenaBaseOffset) on amd64 before computing indexes into
	// the heap arenas index. In 2017, amd64 hardware added
	// support for 57 bit addresses; however, currently only Linux
	// supports this extension and the kernel will never choose an
	// address above 1<<47 unless mmap is called with a hint
	// address above 1<<47 (which we never do).
	//
	// arm64 hardware (as of ARMv8) limits user addresses to 48
	// bits, in the range [0, 1<<48).
	//
	// ppc64, mips64, and s390x support arbitrary 64 bit addresses
	// in hardware. On Linux, Go leans on stricter OS limits. Based
	// on Linux's processor.h, the user address space is limited as
	// follows on 64-bit architectures:
	//
	// Architecture  Name              Maximum Value (exclusive)
	// ---------------------------------------------------------------------
	// amd64         TASK_SIZE_MAX     0x007ffffffff000 (47 bit addresses)
	// arm64         TASK_SIZE_64      0x01000000000000 (48 bit addresses)
	// ppc64{,le}    TASK_SIZE_USER64  0x00400000000000 (46 bit addresses)
	// mips64{,le}   TASK_SIZE64       0x00010000000000 (40 bit addresses)
	// s390x         TASK_SIZE         1<<64 (64 bit addresses)
	//
	// These limits may increase over time, but are currently at
	// most 48 bits except on s390x. On all architectures, Linux
	// starts placing mmap'd regions at addresses that are
	// significantly below 48 bits, so even if it's possible to
	// exceed Go's 48 bit limit, it's extremely unlikely in
	// practice.
	//
	// On aix/ppc64, the limits is increased to 1<<60 to accept addresses
	// returned by mmap syscall. These are in range:
	//  0x0a00000000000000 - 0x0afffffffffffff
	//
	// On 32-bit platforms, we accept the full 32-bit address
	// space because doing so is cheap.
	// mips32 only has access to the low 2GB of virtual memory, so
	// we further limit it to 31 bits.
	//
	// WebAssembly currently has a limit of 4GB linear memory.
	heapAddrBits = (_64bit*(1-sys.GoarchWasm)*(1-sys.GoosAix))*48 + (1-_64bit+sys.GoarchWasm)*(32-(sys.GoarchMips+sys.GoarchMipsle)) + 60*sys.GoosAix

	// maxAlloc is the maximum size of an allocation. On 64-bit,
	// it's theoretically possible to allocate 1<<heapAddrBits bytes. On
	// 32-bit, however, this is one less than 1<<32 because the
	// number of bytes in the address space doesn't actually fit
	// in a uintptr.
	maxAlloc = (1 << heapAddrBits) - (1-_64bit)*1

	// The number of bits in a heap address, the size of heap
	// arenas, and the L1 and L2 arena map sizes are related by
	//
	//   (1 << addr bits) = arena size * L1 entries * L2 entries
	//
	// Currently, we balance these as follows:
	//
	//       Platform  Addr bits  Arena size  L1 entries   L2 entries
	// --------------  ---------  ----------  ----------  -----------
	//       */64-bit         48        64MB           1    4M (32MB)
	//     aix/64-bit         60       256MB        4096    4M (32MB)
	// windows/64-bit         48         4MB          64    1M  (8MB)
	//       */32-bit         32         4MB           1  1024  (4KB)
	//     */mips(le)         31         4MB           1   512  (2KB)

	// heapArenaBytes 为 heap arena 的大小。
	// heap 由大小为 heapArenaBytes 的映射构成，与 heapArenaBytes 对齐。
	// 初始的 heap 只有一个 arena。
	//
	// This is currently 64MB on 64-bit non-Windows and 4MB on
	// 32-bit and on Windows. We use smaller arenas on Windows
	// because all committed memory is charged to the process,
	// even if it's not touched. Hence, for processes with small
	// heaps, the mapped arena space needs to be commensurate.
	// This is particularly important with the race detector,
	// since it significantly amplifies the cost of committed
	// memory.
	//
	heapArenaBytes = 1 << logHeapArenaBytes

	// logHeapArenaBytes is log_2 of heapArenaBytes. For clarity,
	// prefer using heapArenaBytes where possible (we need the
	// constant to compute some other constants).
	logHeapArenaBytes = (6+20)*(_64bit*(1-sys.GoosWindows)*(1-sys.GoosAix)) + (2+20)*(_64bit*sys.GoosWindows) + (2+20)*(1-_64bit) + (8+20)*sys.GoosAix

	// heapArenaBitmapBytes 为每个 heap arena 的 bitmap 的大小
	heapArenaBitmapBytes = heapArenaBytes / (sys.PtrSize * 8 / 2)

	pagesPerArena = heapArenaBytes / pageSize

	// arenaL1Bits is the number of bits of the arena number
	// covered by the first level arena map.
	//
	// This number should be small, since the first level arena
	// map requires PtrSize*(1<<arenaL1Bits) of space in the
	// binary's BSS. It can be zero, in which case the first level
	// index is effectively unused. There is a performance benefit
	// to this, since the generated code can be more efficient,
	// but comes at the cost of having a large L2 mapping.
	//
	// We use the L1 map on 64-bit Windows because the arena size
	// is small, but the address space is still 48 bits, and
	// there's a high cost to having a large L2.
	//
	// We use the L1 map on aix/ppc64 to keep the same L2 value
	// as on Linux.
	arenaL1Bits = 6*(_64bit*sys.GoosWindows) + 12*sys.GoosAix
	// arenaL2Bits is the number of bits of the arena number
	// covered by the second level arena index.
	//
	// The size of each arena map allocation is proportional to
	// 1<<arenaL2Bits, so it's important that this not be too
	// large. 48 bits leads to 32MB arena index allocations, which
	// is about the practical threshold.
	arenaL2Bits = heapAddrBits - logHeapArenaBytes - arenaL1Bits

	// arenaL1Shift is the number of bits to shift an arena frame
	// number by to compute an index into the first level arena map.
	arenaL1Shift = arenaL2Bits

	// arenaBits is the total bits in a combined arena map index.
	// This is split between the index into the L1 arena map and
	// the L2 arena map.
	arenaBits = arenaL1Bits + arenaL2Bits

	// arenaBaseOffset is the pointer value that corresponds to
	// index 0 in the heap arena map.
	//
	// On amd64, the address space is 48 bits, sign extended to 64
	// bits. This offset lets us handle "negative" addresses (or
	// high addresses if viewed as unsigned).
	//
	// On other platforms, the user address space is contiguous
	// and starts at 0, so no offset is necessary.
	arenaBaseOffset uintptr = sys.GoarchAmd64 * (1 << 47)

	// Max number of threads to run garbage collection.
	// 2, 3, and 4 are all plausible maximums depending
	// on the hardware details of the machine. The garbage
	// collector scales well to 32 cpus.
	_MaxGcproc = 32

	// minLegalPointer is the smallest possible legal pointer.
	// This is the smallest possible architectural page size,
	// since we assume that the first page is never mapped.
	//
	// This should agree with minZeroPage in the compiler.
	minLegalPointer uintptr = 4096
)

// physPageSize 是操作系统的物理页字节大小。内存页的映射和反映射操作必须以
// physPageSize 的整数倍完成
//
// 它必须在操作系统初始化代码中、mallocinit 之前进行设置（通常在 osinit）
var physPageSize uintptr

// OS-defined helpers: 操作系统级别的辅助函数
//
// sysAlloc 从操作系统上获得大量清零的内存，通常大小约为 KB 或者 MB 级别.
// 注意: sysAlloc 返回的是操作系统排布的内存，但堆分配器可能使用更大的布局。
// 因此调用方必须谨慎的重排从 sysAlloc 获得的内存。
//
// sysUnused notifies the operating system that the contents
// of the memory region are no longer needed and can be reused
// for other purposes.
// sysUsed notifies the operating system that the contents
// of the memory region are needed again.
//
// sysFree returns it unconditionally; this is only used if
// an out-of-memory error has been detected midway through
// an allocation. It is okay if sysFree is a no-op.
//
// sysReserve reserves address space without allocating memory.
// If the pointer passed to it is non-nil, the caller wants the
// reservation there, but sysReserve can still choose another
// location if that one is unavailable.
// NOTE: sysReserve returns OS-aligned memory, but the heap allocator
// may use larger alignment, so the caller must be careful to realign the
// memory obtained by sysAlloc.
//
// sysMap maps previously reserved address space for use.
//
// sysFault marks a (already sysAlloc'd) region to fault
// if accessed. Used only for debugging the runtime.

func mallocinit() {
	if class_to_size[_TinySizeClass] != _TinySize {
		throw("bad TinySizeClass")
	}

	testdefersizes()

	if heapArenaBitmapBytes&(heapArenaBitmapBytes-1) != 0 {
		// heapBits expects modular arithmetic on bitmap
		// addresses to work.
		throw("heapArenaBitmapBytes not a power of 2")
	}

	// 将 class size 复制到统计表中
	for i := range class_to_size {
		memstats.by_size[i].size = uint32(class_to_size[i])
	}

	// 检查 physPageSize
	if physPageSize == 0 {
		// 获取系统物理 page 大小失败
		throw("failed to get system page size")
	}
	// 如果 page 太小也失败 4KB
	if physPageSize < minPhysPageSize {
		print("system page size (", physPageSize, ") is smaller than minimum page size (", minPhysPageSize, ")\n")
		throw("bad system page size")
	}
	// 系统 page 大小必须是 2 的任意指数大小
	if physPageSize&(physPageSize-1) != 0 {
		print("system page size (", physPageSize, ") must be a power of 2\n")
		throw("bad system page size")
	}

	// 初始化堆
	mheap_.init()
	_g_ := getg()
	_g_.m.mcache = allocmcache()

	// 创建初始的 arena 增长 hint
	if sys.PtrSize == 8 && GOARCH != "wasm" {
		// 64 位机器上，我们选取下面的 hint，因为：
		//
		// 1.从地址空间的中间开始，可以更容易地扩展连续范围，而无需进入其他映射。
		//
		// 2. 这使得 Go 堆地址在调试时更容易识别。
		//
		// 3. gccgo 中的堆栈扫描仍然是保守型（conservative）的，因此地址与其他数据的区别开来非常重要。
		//
		// 从 0x00c0 开始表示有效内存地址从 0x00c0, 0x00c1, ...
		// 在小端（little-endian）表示中，即为 c0 00, c1 00, ... 这些都是无效的 UTF-8 序列，
		// 并且他们尽可能的里 ff （像一个通用的 byte）远。如果失败，我们尝试 0xXXc0 地址。
		// 早些时候尝试使用 0x11f8 会在 OS X 上当执行线程分配时导致内存不足错误。
		// 0c00c0 会与 AddressSanitizer 产生冲突，后者保留从起开始到 0x0100 的所有内存。
		// 这些选择降低了一个保守型垃圾回收器不回收内存的概率，因为某些非指针内存块具有与
		// 内存地址相匹配的位模式。
		//
		// 但是，在 arm64 中，当使用 4K 大小具有三级的转换缓冲区的页（page）时，用户地址空间
		// 被限制在了 39 bit，因此我们忽略了上面所有的建议并强制分配在 0x40 << 32 上。
		// 在 darwin/arm64 中，地址空间甚至更小。
		// 在 AIX 64 位系统中, mmaps 从 0x0A00000000000000 开始处理。
		//
		// 从 0xc000000000 开始设置保留地址
		// 如果失败，则尝试 0x1c000000000 ~ 0x7fc000000000
		for i := 0x7f; i >= 0; i-- {
			var p uintptr
			switch {
			case GOARCH == "arm64" && GOOS == "darwin":
				p = uintptr(i)<<40 | uintptrMask&(0x0013<<28)
			case GOARCH == "arm64":
				p = uintptr(i)<<40 | uintptrMask&(0x0040<<32)
			case GOOS == "aix":
				if i == 0 {
					// We don't use addresses directly after 0x0A00000000000000
					// to avoid collisions with others mmaps done by non-go programs.
					continue
				}
				p = uintptr(i)<<40 | uintptrMask&(0xa0<<52)
			case raceenabled:
				// TSAN 运行时需要堆地址在 [0x00c000000000, 0x00e000000000) 范围内
				p = uintptr(i)<<32 | uintptrMask&(0x00c0<<32)
				if p >= uintptrMask&0x00e000000000 {
					continue
				}
			default:
				p = uintptr(i)<<40 | uintptrMask&(0x00c0<<32)
			}
			hint := (*arenaHint)(mheap_.arenaHintAlloc.alloc())
			hint.addr = p
			hint.next, mheap_.arenaHints = mheap_.arenaHints, hint
		}
	} else {
		// On a 32-bit machine, we're much more concerned
		// about keeping the usable heap contiguous.
		// Hence:
		//
		// 1. We reserve space for all heapArenas up front so
		// they don't get interleaved with the heap. They're
		// ~258MB, so this isn't too bad. (We could reserve a
		// smaller amount of space up front if this is a
		// problem.)
		//
		// 2. We hint the heap to start right above the end of
		// the binary so we have the best chance of keeping it
		// contiguous.
		//
		// 3. We try to stake out a reasonably large initial
		// heap reservation.

		const arenaMetaSize = (1 << arenaBits) * unsafe.Sizeof(heapArena{})
		meta := uintptr(sysReserve(nil, arenaMetaSize))
		if meta != 0 {
			mheap_.heapArenaAlloc.init(meta, arenaMetaSize)
		}

		// We want to start the arena low, but if we're linked
		// against C code, it's possible global constructors
		// have called malloc and adjusted the process' brk.
		// Query the brk so we can avoid trying to map the
		// region over it (which will cause the kernel to put
		// the region somewhere else, likely at a high
		// address).
		procBrk := sbrk0()

		// If we ask for the end of the data segment but the
		// operating system requires a little more space
		// before we can start allocating, it will give out a
		// slightly higher pointer. Except QEMU, which is
		// buggy, as usual: it won't adjust the pointer
		// upward. So adjust it upward a little bit ourselves:
		// 1/4 MB to get away from the running binary image.
		p := firstmoduledata.end
		if p < procBrk {
			p = procBrk
		}
		if mheap_.heapArenaAlloc.next <= p && p < mheap_.heapArenaAlloc.end {
			p = mheap_.heapArenaAlloc.end
		}
		p = round(p+(256<<10), heapArenaBytes)
		// Because we're worried about fragmentation on
		// 32-bit, we try to make a large initial reservation.
		arenaSizes := []uintptr{
			512 << 20,
			256 << 20,
			128 << 20,
		}
		for _, arenaSize := range arenaSizes {
			a, size := sysReserveAligned(unsafe.Pointer(p), arenaSize, heapArenaBytes)
			if a != nil {
				mheap_.arena.init(uintptr(a), size)
				p = uintptr(a) + size // For hint below
				break
			}
		}
		hint := (*arenaHint)(mheap_.arenaHintAlloc.alloc())
		hint.addr = p
		hint.next, mheap_.arenaHints = mheap_.arenaHints, hint
	}
}

// sysAlloc allocates heap arena space for at least n bytes. The
// returned pointer is always heapArenaBytes-aligned and backed by
// h.arenas metadata. The returned size is always a multiple of
// heapArenaBytes. sysAlloc returns nil on failure.
// There is no corresponding free function.
//
// h must be locked.
func (h *mheap) sysAlloc(n uintptr) (v unsafe.Pointer, size uintptr) {
	n = round(n, heapArenaBytes)

	// First, try the arena pre-reservation.
	v = h.arena.alloc(n, heapArenaBytes, &memstats.heap_sys)
	if v != nil {
		size = n
		goto mapped
	}

	// Try to grow the heap at a hint address.
	for h.arenaHints != nil {
		hint := h.arenaHints
		p := hint.addr
		if hint.down {
			p -= n
		}
		if p+n < p {
			// We can't use this, so don't ask.
			v = nil
		} else if arenaIndex(p+n-1) >= 1<<arenaBits {
			// Outside addressable heap. Can't use.
			v = nil
		} else {
			v = sysReserve(unsafe.Pointer(p), n)
		}
		if p == uintptr(v) {
			// Success. Update the hint.
			if !hint.down {
				p += n
			}
			hint.addr = p
			size = n
			break
		}
		// Failed. Discard this hint and try the next.
		//
		// TODO: This would be cleaner if sysReserve could be
		// told to only return the requested address. In
		// particular, this is already how Windows behaves, so
		// it would simply things there.
		if v != nil {
			sysFree(v, n, nil)
		}
		h.arenaHints = hint.next
		h.arenaHintAlloc.free(unsafe.Pointer(hint))
	}

	if size == 0 {
		if raceenabled {
			// The race detector assumes the heap lives in
			// [0x00c000000000, 0x00e000000000), but we
			// just ran out of hints in this region. Give
			// a nice failure.
			throw("too many address space collisions for -race mode")
		}

		// All of the hints failed, so we'll take any
		// (sufficiently aligned) address the kernel will give
		// us.
		v, size = sysReserveAligned(nil, n, heapArenaBytes)
		if v == nil {
			return nil, 0
		}

		// Create new hints for extending this region.
		hint := (*arenaHint)(h.arenaHintAlloc.alloc())
		hint.addr, hint.down = uintptr(v), true
		hint.next, mheap_.arenaHints = mheap_.arenaHints, hint
		hint = (*arenaHint)(h.arenaHintAlloc.alloc())
		hint.addr = uintptr(v) + size
		hint.next, mheap_.arenaHints = mheap_.arenaHints, hint
	}

	// Check for bad pointers or pointers we can't use.
	{
		var bad string
		p := uintptr(v)
		if p+size < p {
			bad = "region exceeds uintptr range"
		} else if arenaIndex(p) >= 1<<arenaBits {
			bad = "base outside usable address space"
		} else if arenaIndex(p+size-1) >= 1<<arenaBits {
			bad = "end outside usable address space"
		}
		if bad != "" {
			// This should be impossible on most architectures,
			// but it would be really confusing to debug.
			print("runtime: memory allocated by OS [", hex(p), ", ", hex(p+size), ") not in usable address space: ", bad, "\n")
			throw("memory reservation exceeds address space limit")
		}
	}

	if uintptr(v)&(heapArenaBytes-1) != 0 {
		throw("misrounded allocation in sysAlloc")
	}

	// Back the reservation.
	sysMap(v, size, &memstats.heap_sys)

mapped:
	// Create arena metadata.
	for ri := arenaIndex(uintptr(v)); ri <= arenaIndex(uintptr(v)+size-1); ri++ {
		l2 := h.arenas[ri.l1()]
		if l2 == nil {
			// Allocate an L2 arena map.
			l2 = (*[1 << arenaL2Bits]*heapArena)(persistentalloc(unsafe.Sizeof(*l2), sys.PtrSize, nil))
			if l2 == nil {
				throw("out of memory allocating heap arena map")
			}
			atomic.StorepNoWB(unsafe.Pointer(&h.arenas[ri.l1()]), unsafe.Pointer(l2))
		}

		if l2[ri.l2()] != nil {
			throw("arena already initialized")
		}
		var r *heapArena
		r = (*heapArena)(h.heapArenaAlloc.alloc(unsafe.Sizeof(*r), sys.PtrSize, &memstats.gc_sys))
		if r == nil {
			r = (*heapArena)(persistentalloc(unsafe.Sizeof(*r), sys.PtrSize, &memstats.gc_sys))
			if r == nil {
				throw("out of memory allocating heap arena metadata")
			}
		}

		// Add the arena to the arenas list.
		if len(h.allArenas) == cap(h.allArenas) {
			size := 2 * uintptr(cap(h.allArenas)) * sys.PtrSize
			if size == 0 {
				size = physPageSize
			}
			newArray := (*notInHeap)(persistentalloc(size, sys.PtrSize, &memstats.gc_sys))
			if newArray == nil {
				throw("out of memory allocating allArenas")
			}
			oldSlice := h.allArenas
			*(*notInHeapSlice)(unsafe.Pointer(&h.allArenas)) = notInHeapSlice{newArray, len(h.allArenas), int(size / sys.PtrSize)}
			copy(h.allArenas, oldSlice)
			// Do not free the old backing array because
			// there may be concurrent readers. Since we
			// double the array each time, this can lead
			// to at most 2x waste.
		}
		h.allArenas = h.allArenas[:len(h.allArenas)+1]
		h.allArenas[len(h.allArenas)-1] = ri

		// Store atomically just in case an object from the
		// new heap arena becomes visible before the heap lock
		// is released (which shouldn't happen, but there's
		// little downside to this).
		atomic.StorepNoWB(unsafe.Pointer(&l2[ri.l2()]), unsafe.Pointer(r))
	}

	// Tell the race detector about the new heap memory.
	if raceenabled {
		racemapshadow(v, size)
	}

	return
}

// sysReserveAligned is like sysReserve, but the returned pointer is
// aligned to align bytes. It may reserve either n or n+align bytes,
// so it returns the size that was reserved.
func sysReserveAligned(v unsafe.Pointer, size, align uintptr) (unsafe.Pointer, uintptr) {
	// Since the alignment is rather large in uses of this
	// function, we're not likely to get it by chance, so we ask
	// for a larger region and remove the parts we don't need.
	retries := 0
retry:
	p := uintptr(sysReserve(v, size+align))
	switch {
	case p == 0:
		return nil, 0
	case p&(align-1) == 0:
		// We got lucky and got an aligned region, so we can
		// use the whole thing.
		return unsafe.Pointer(p), size + align
	case GOOS == "windows":
		// On Windows we can't release pieces of a
		// reservation, so we release the whole thing and
		// re-reserve the aligned sub-region. This may race,
		// so we may have to try again.
		sysFree(unsafe.Pointer(p), size+align, nil)
		p = round(p, align)
		p2 := sysReserve(unsafe.Pointer(p), size)
		if p != uintptr(p2) {
			// Must have raced. Try again.
			sysFree(p2, size, nil)
			if retries++; retries == 100 {
				throw("failed to allocate aligned heap memory; too many retries")
			}
			goto retry
		}
		// Success.
		return p2, size
	default:
		// Trim off the unaligned parts.
		pAligned := round(p, align)
		sysFree(unsafe.Pointer(p), pAligned-p, nil)
		end := pAligned + size
		endLen := (p + size + align) - end
		if endLen > 0 {
			sysFree(unsafe.Pointer(end), endLen, nil)
		}
		return unsafe.Pointer(pAligned), size
	}
}

// 适用于所有的 0 字节分配的基地址
var zerobase uintptr

// nextFreeFast returns the next free object if one is quickly available.
// Otherwise it returns 0.
func nextFreeFast(s *mspan) gclinkptr {
	theBit := sys.Ctz64(s.allocCache) // Is there a free object in the allocCache?
	if theBit < 64 {
		result := s.freeindex + uintptr(theBit)
		if result < s.nelems {
			freeidx := result + 1
			if freeidx%64 == 0 && freeidx != s.nelems {
				return 0
			}
			s.allocCache >>= uint(theBit + 1)
			s.freeindex = freeidx
			s.allocCount++
			return gclinkptr(result*s.elemsize + s.base())
		}
	}
	return 0
}

// nextFree returns the next free object from the cached span if one is available.
// Otherwise it refills the cache with a span with an available object and
// returns that object along with a flag indicating that this was a heavy
// weight allocation. If it is a heavy weight allocation the caller must
// determine whether a new GC cycle needs to be started or if the GC is active
// whether this goroutine needs to assist the GC.
//
// Must run in a non-preemptible context since otherwise the owner of
// c could change.
func (c *mcache) nextFree(spc spanClass) (v gclinkptr, s *mspan, shouldhelpgc bool) {
	s = c.alloc[spc]
	shouldhelpgc = false
	freeIndex := s.nextFreeIndex()
	if freeIndex == s.nelems {
		// The span is full.
		if uintptr(s.allocCount) != s.nelems {
			println("runtime: s.allocCount=", s.allocCount, "s.nelems=", s.nelems)
			throw("s.allocCount != s.nelems && freeIndex == s.nelems")
		}
		c.refill(spc)
		shouldhelpgc = true
		s = c.alloc[spc]

		freeIndex = s.nextFreeIndex()
	}

	if freeIndex >= s.nelems {
		throw("freeIndex is not valid")
	}

	v = gclinkptr(freeIndex*s.elemsize + s.base())
	s.allocCount++
	if uintptr(s.allocCount) > s.nelems {
		println("s.allocCount=", s.allocCount, "s.nelems=", s.nelems)
		throw("s.allocCount > s.nelems")
	}
	return
}

// 分配大小为字节的对象。
// 从每个 P 缓存的空闲列表中分配小对象。
// 大对象（> 32 kB）直接从堆中分配。
func mallocgc(size uintptr, typ *_type, needzero bool) unsafe.Pointer {
	if gcphase == _GCmarktermination {
		throw("mallocgc called with gcphase == _GCmarktermination")
	}

	// 创建大小为零的对象，例如空结构体
	if size == 0 {
		return unsafe.Pointer(&zerobase)
	}

	if debug.sbrk != 0 {
		align := uintptr(16)
		if typ != nil {
			align = uintptr(typ.align)
		}
		return persistentalloc(size, align, &memstats.other_sys)
	}

	// assistG is the G to charge for this allocation, or nil if
	// GC is not currently active.
	var assistG *g
	if gcBlackenEnabled != 0 {
		// Charge the current user G for this allocation.
		assistG = getg()
		if assistG.m.curg != nil {
			assistG = assistG.m.curg
		}
		// Charge the allocation against the G. We'll account
		// for internal fragmentation at the end of mallocgc.
		assistG.gcAssistBytes -= int64(size)

		if assistG.gcAssistBytes < 0 {
			// This G is in debt. Assist the GC to correct
			// this before allocating. This must happen
			// before disabling preemption.
			gcAssistAlloc(assistG)
		}
	}

	// Set mp.mallocing to keep from being preempted by GC.
	mp := acquirem()
	if mp.mallocing != 0 {
		throw("malloc deadlock")
	}
	if mp.gsignal == getg() {
		throw("malloc during signal")
	}
	mp.mallocing = 1

	shouldhelpgc := false
	dataSize := size
	// 获取 mcache
	c := gomcache()
	var x unsafe.Pointer
	noscan := typ == nil || typ.kind&kindNoPointers != 0
	if size <= maxSmallSize {
		if noscan && size < maxTinySize {
			// 微型分配器 (tiny)
			//
			// 微型分配器结合了多个的微型分配请求，并将其合并到一个单一的内存块中。
			// 微型分配器将几个微小的分配请求组合到一个内存块中。
			// 当所有子对象都无法访问时，将释放生成的内存块。子对象必须是 noscan（没有指针），
			// 这可以确保可能浪费的内存量是有界的。
			//
			// 用于组合的内存块的大小（maxTinySize）是可调的。
			// 当前设置为 16 个字节，这与 2x 最坏情况的内存浪费
			//（当除了一个子对象之外的所有子对象都无法访问时）有关。
			// 8 字节虽然可以完全没有浪费，但会提供更少的组合机会。
			// 32 字节提供了更多的组合机会，但可能导致 4 倍最坏情况下的浪费。
			// 无论块大小如何，最好的做法是采用 8 倍。
			//
			// 从微型分配器获得的对象不得明确释放。
			// 因此，当显式释放对象时，我们确保其大小 >= maxTinySize。
			//
			// SetFinalizer 对于可能来自微型分配器的对象有一个特殊情况，
			// 它允许为内存块的内部字节设置 finalizer。
			//
			// 微型分配器的主要目标是小字符串和独立的逃逸变量。
			// 在 json 性能测试中，分配器将分配次数减少了大约 12％，并将堆大小占用减少了大约 20％。
			off := c.tinyoffset
			// Align tiny pointer for required (conservative) alignment.
			if size&7 == 0 {
				off = round(off, 8)
			} else if size&3 == 0 {
				off = round(off, 4)
			} else if size&1 == 0 {
				off = round(off, 2)
			}
			if off+size <= maxTinySize && c.tiny != 0 {
				// The object fits into existing tiny block.
				x = unsafe.Pointer(c.tiny + off)
				c.tinyoffset = off + size
				c.local_tinyallocs++
				mp.mallocing = 0
				releasem(mp)
				return x
			}
			// Allocate a new maxTinySize block.
			span := c.alloc[tinySpanClass]
			v := nextFreeFast(span)
			if v == 0 {
				v, _, shouldhelpgc = c.nextFree(tinySpanClass)
			}
			x = unsafe.Pointer(v)
			(*[2]uint64)(x)[0] = 0
			(*[2]uint64)(x)[1] = 0
			// See if we need to replace the existing tiny block with the new one
			// based on amount of remaining free space.
			if size < c.tinyoffset || c.tiny == 0 {
				c.tiny = uintptr(x)
				c.tinyoffset = size
			}
			size = maxTinySize
		} else {
			var sizeclass uint8
			if size <= smallSizeMax-8 {
				sizeclass = size_to_class8[(size+smallSizeDiv-1)/smallSizeDiv]
			} else {
				sizeclass = size_to_class128[(size-smallSizeMax+largeSizeDiv-1)/largeSizeDiv]
			}
			size = uintptr(class_to_size[sizeclass])
			spc := makeSpanClass(sizeclass, noscan)
			span := c.alloc[spc]
			v := nextFreeFast(span)
			if v == 0 {
				v, span, shouldhelpgc = c.nextFree(spc)
			}
			x = unsafe.Pointer(v)
			if needzero && span.needzero != 0 {
				memclrNoHeapPointers(unsafe.Pointer(v), size)
			}
		}
	} else {
		var s *mspan
		shouldhelpgc = true
		systemstack(func() {
			s = largeAlloc(size, needzero, noscan)
		})
		s.freeindex = 1
		s.allocCount = 1
		x = unsafe.Pointer(s.base())
		size = s.elemsize
	}

	var scanSize uintptr
	if !noscan {
		// If allocating a defer+arg block, now that we've picked a malloc size
		// large enough to hold everything, cut the "asked for" size down to
		// just the defer header, so that the GC bitmap will record the arg block
		// as containing nothing at all (as if it were unused space at the end of
		// a malloc block caused by size rounding).
		// The defer arg areas are scanned as part of scanstack.
		if typ == deferType {
			dataSize = unsafe.Sizeof(_defer{})
		}
		heapBitsSetType(uintptr(x), size, dataSize, typ)
		if dataSize > typ.size {
			// Array allocation. If there are any
			// pointers, GC has to scan to the last
			// element.
			if typ.ptrdata != 0 {
				scanSize = dataSize - typ.size + typ.ptrdata
			}
		} else {
			scanSize = typ.ptrdata
		}
		c.local_scan += scanSize
	}

	// Ensure that the stores above that initialize x to
	// type-safe memory and set the heap bits occur before
	// the caller can make x observable to the garbage
	// collector. Otherwise, on weakly ordered machines,
	// the garbage collector could follow a pointer to x,
	// but see uninitialized memory or stale heap bits.
	publicationBarrier()

	// Allocate black during GC.
	// All slots hold nil so no scanning is needed.
	// This may be racing with GC so do it atomically if there can be
	// a race marking the bit.
	if gcphase != _GCoff {
		gcmarknewobject(uintptr(x), size, scanSize)
	}

	if raceenabled {
		racemalloc(x, size)
	}

	if msanenabled {
		msanmalloc(x, size)
	}

	mp.mallocing = 0
	releasem(mp)

	if debug.allocfreetrace != 0 {
		tracealloc(x, size, typ)
	}

	if rate := MemProfileRate; rate > 0 {
		if rate != 1 && int32(size) < c.next_sample {
			c.next_sample -= int32(size)
		} else {
			mp := acquirem()
			profilealloc(mp, x, size)
			releasem(mp)
		}
	}

	if assistG != nil {
		// Account for internal fragmentation in the assist
		// debt now that we know it.
		assistG.gcAssistBytes -= int64(size - dataSize)
	}

	if shouldhelpgc {
		if t := (gcTrigger{kind: gcTriggerHeap}); t.test() {
			gcStart(t)
		}
	}

	return x
}

func largeAlloc(size uintptr, needzero bool, noscan bool) *mspan {
	// print("largeAlloc size=", size, "\n")

	// 对象太大，溢出
	if size+_PageSize < size {
		throw("out of memory")
	}
	// 根据分配的大小计算需要分配的页数
	npages := size >> _PageShift
	if size&_PageMask != 0 {
		npages++
	}

	// Deduct credit for this span allocation and sweep if
	// necessary. mHeap_Alloc will also sweep npages, so this only
	// pays the debt down to npage pages.
	deductSweepCredit(npages*_PageSize, npages)

	// 从堆上分配
	s := mheap_.alloc(npages, makeSpanClass(0, noscan), true, needzero)
	if s == nil {
		throw("out of memory")
	}
	s.limit = s.base() + size
	heapBitsForAddr(s.base()).initSpan(s)
	return s
}

// new 关键字的实现，编译器（前端和SSA后端）的实现知道此函数的签名
func newobject(typ *_type) unsafe.Pointer {
	return mallocgc(typ.size, typ, true)
}

//go:linkname reflect_unsafe_New reflect.unsafe_New
func reflect_unsafe_New(typ *_type) unsafe.Pointer {
	return mallocgc(typ.size, typ, true)
}

// newarray allocates an array of n elements of type typ.
func newarray(typ *_type, n int) unsafe.Pointer {
	if n == 1 {
		return mallocgc(typ.size, typ, true)
	}
	mem, overflow := math.MulUintptr(typ.size, uintptr(n))
	if overflow || mem > maxAlloc || n < 0 {
		panic(plainError("runtime: allocation size out of range"))
	}
	return mallocgc(mem, typ, true)
}

//go:linkname reflect_unsafe_NewArray reflect.unsafe_NewArray
func reflect_unsafe_NewArray(typ *_type, n int) unsafe.Pointer {
	return newarray(typ, n)
}

func profilealloc(mp *m, x unsafe.Pointer, size uintptr) {
	mp.mcache.next_sample = nextSample()
	mProf_Malloc(x, size)
}

// nextSample 返回堆分析的下一个采样点。目标是平均每个 MemProfileRate 字节采样分配，
// 但在分配时间线上具有完全随机的分布；这对应于带有参数 MemProfileRate 的泊松过程。
// 在泊松过程中，两个样本之间的距离遵循指数分布（exp(MemProfileRate)），
// 因此最佳返回值是取自指数分布的随机数，其均值为 MemProfileRate。
func nextSample() int32 {
	if GOOS == "plan9" {
		// Plan 9 doesn't support floating point in note handler.
		if g := getg(); g == g.m.gsignal {
			return nextSampleNoFP()
		}
	}

	return fastexprand(MemProfileRate) // exp(MemProfileRate)
}

// fastexprand returns a random number from an exponential distribution with
// the specified mean.
func fastexprand(mean int) int32 {
	// Avoid overflow. Maximum possible step is
	// -ln(1/(1<<randomBitCount)) * mean, approximately 20 * mean.
	switch {
	case mean > 0x7000000:
		mean = 0x7000000
	case mean == 0:
		return 0
	}

	// Take a random sample of the exponential distribution exp(-mean*x).
	// The probability distribution function is mean*exp(-mean*x), so the CDF is
	// p = 1 - exp(-mean*x), so
	// q = 1 - p == exp(-mean*x)
	// log_e(q) = -mean*x
	// -log_e(q)/mean = x
	// x = -log_e(q) * mean
	// x = log_2(q) * (-log_e(2)) * mean    ; Using log_2 for efficiency
	const randomBitCount = 26
	q := fastrand()%(1<<randomBitCount) + 1
	qlog := fastlog2(float64(q)) - randomBitCount
	if qlog > 0 {
		qlog = 0
	}
	const minusLog2 = -0.6931471805599453 // -ln(2)
	return int32(qlog*(minusLog2*float64(mean))) + 1
}

// nextSampleNoFP is similar to nextSample, but uses older,
// simpler code to avoid floating point.
func nextSampleNoFP() int32 {
	// Set first allocation sample size.
	rate := MemProfileRate
	if rate > 0x3fffffff { // make 2*rate not overflow
		rate = 0x3fffffff
	}
	if rate != 0 {
		return int32(fastrand() % uint32(2*rate))
	}
	return 0
}

type persistentAlloc struct {
	base *notInHeap // 空结构，内存首地址
	off  uintptr    // 偏移量
}

var globalAlloc struct {
	mutex
	persistentAlloc
}

// persistentChunkSize is the number of bytes we allocate when we grow
// a persistentAlloc.
const persistentChunkSize = 256 << 10

// persistentChunks is a list of all the persistent chunks we have
// allocated. The list is maintained through the first word in the
// persistent chunk. This is updated atomically.
var persistentChunks *notInHeap

// sysAlloc 的一层封装，能够分配小的 chunk
// 没有释放操作
// 用于 函数、类型、调试相关的持久型数据分配。
// 如果 align 为 0 则使用默认对其（当前为 8）
// 返回的内存会被清零
//
// 考虑使此函数类型为 go:notinheap
func persistentalloc(size, align uintptr, sysStat *uint64) unsafe.Pointer {
	var p *notInHeap
	systemstack(func() {
		p = persistentalloc1(size, align, sysStat)
	})
	return unsafe.Pointer(p)
}

// 必须在系统栈上执行，因为栈增长可以（再）调用它。见 issue 9174
//go:systemstack
func persistentalloc1(size, align uintptr, sysStat *uint64) *notInHeap {
	const (
		maxBlock = 64 << 10 // VM reservation granularity is 64K on windows
	)

	// 不允许分配大小为 0 的空间
	if size == 0 {
		throw("persistentalloc: size == 0")
	}
	// 对齐数必须为 2 的指数、且不大于 PageSize
	if align != 0 {
		if align&(align-1) != 0 {
			throw("persistentalloc: align is not a power of 2")
		}
		if align > _PageSize {
			throw("persistentalloc: align is too large")
		}
	} else {
		// 若未指定则默认为 8
		align = 8
	}

	// 分配大内存：分配的大小如果超过最大的 block 大小，则直接调用 sysAlloc 进行分配
	if size >= maxBlock {
		return (*notInHeap)(sysAlloc(size, sysStat))
	}

	// 分配小内存：在 m 上进行
	// 先获取 m
	mp := acquirem()
	var persistent *persistentAlloc
	if mp != nil && mp.p != 0 { // 如果能够获取到 m 且同时持有 p，则直接分配到 p 的 palloc 上
		persistent = &mp.p.ptr().palloc
	} else { // 否则就分配到全局的 globalAlloc.persistentAlloc 上
		lock(&globalAlloc.mutex)
		persistent = &globalAlloc.persistentAlloc
	}
	// 四舍五入 off 到 align 的倍数
	persistent.off = round(persistent.off, align)
	if persistent.off+size > persistentChunkSize || persistent.base == nil {
		persistent.base = (*notInHeap)(sysAlloc(persistentChunkSize, &memstats.other_sys))
		if persistent.base == nil {
			if persistent == &globalAlloc.persistentAlloc {
				unlock(&globalAlloc.mutex)
			}
			throw("runtime: cannot allocate memory")
		}

		// Add the new chunk to the persistentChunks list.
		for {
			chunks := uintptr(unsafe.Pointer(persistentChunks))
			*(*uintptr)(unsafe.Pointer(persistent.base)) = chunks
			if atomic.Casuintptr((*uintptr)(unsafe.Pointer(&persistentChunks)), chunks, uintptr(unsafe.Pointer(persistent.base))) {
				break
			}
		}
		persistent.off = sys.PtrSize
	}
	p := persistent.base.add(persistent.off)
	persistent.off += size
	releasem(mp)
	if persistent == &globalAlloc.persistentAlloc {
		unlock(&globalAlloc.mutex)
	}

	if sysStat != &memstats.other_sys {
		mSysStatInc(sysStat, size)
		mSysStatDec(&memstats.other_sys, size)
	}
	return p
}

// inPersistentAlloc reports whether p points to memory allocated by
// persistentalloc. This must be nosplit because it is called by the
// cgo checker code, which is called by the write barrier code.
//go:nosplit
func inPersistentAlloc(p uintptr) bool {
	chunk := atomic.Loaduintptr((*uintptr)(unsafe.Pointer(&persistentChunks)))
	for chunk != 0 {
		if p >= chunk && p < chunk+persistentChunkSize {
			return true
		}
		chunk = *(*uintptr)(unsafe.Pointer(chunk))
	}
	return false
}

// linearAlloc 是一个简单的线性分配器，提前储备了内存的一块区域并在需要时指向该区域。
// 调用方负责加锁。
type linearAlloc struct {
	next   uintptr // 下一个可用的字节
	mapped uintptr // 映射空间后的一个字节
	end    uintptr // 保留空间的末尾
}

func (l *linearAlloc) init(base, size uintptr) {
	l.next, l.mapped = base, base
	l.end = base + size
}

func (l *linearAlloc) alloc(size, align uintptr, sysStat *uint64) unsafe.Pointer {
	p := round(l.next, align)
	if p+size > l.end {
		return nil
	}
	l.next = p + size
	if pEnd := round(l.next-1, physPageSize); pEnd > l.mapped {
		// We need to map more of the reserved space.
		sysMap(unsafe.Pointer(l.mapped), pEnd-l.mapped, sysStat)
		l.mapped = pEnd
	}
	return unsafe.Pointer(p)
}

// notInHeap 是类似于 sysAlloc 或 persistentAlloc 等底层分配器分配的堆外内存。
//
// 一般来说，最好使用标记为 go:notinheap 的实际类型，但这对于无法实现的情况
// （如分配器中）则充当泛型类型。
//
// TODO: Use this as the return type of sysAlloc, persistentAlloc, etc?
//
//go:notinheap
type notInHeap struct{}

func (p *notInHeap) add(bytes uintptr) *notInHeap {
	return (*notInHeap)(unsafe.Pointer(uintptr(unsafe.Pointer(p)) + bytes))
}
