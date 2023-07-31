// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package sync provides basic synchronization primitives such as mutual
// exclusion locks. Other than the Once and WaitGroup types, most are intended
// for use by low-level library routines. Higher-level synchronization is
// better done via channels and communication.
//
// Values containing the types defined in this package should not be copied.
package sync

import (
	"internal/race"
	"sync/atomic"
	"unsafe"
)

// Provided by runtime via linkname.
func throw(string)
func fatal(string)

// A Mutex is a mutual exclusion lock.
// The zero value for a Mutex is an unlocked mutex.
//
// A Mutex must not be copied after first use.
//
// In the terminology of the Go memory model,
// the n'th call to Unlock “synchronizes before” the m'th call to Lock
// for any n < m.
// A successful call to TryLock is equivalent to a call to Lock.
// A failed call to TryLock does not establish any “synchronizes before”
// relation at all.
// 锁的结构实现
type Mutex struct {
	// 状态值
	state int32
	// 信号量
	sema uint32
}

// A Locker represents an object that can be locked and unlocked.
type Locker interface {
	Lock()
	Unlock()
}

const (
	// 最右侧第一个bit位，锁的状态位
	// 1-已加锁 0-未加锁
	mutexLocked = 1 << iota // mutex is locked

	// 最右侧第二个bit位，是否有协程在抢锁
	// 表示是否从阻塞队列中唤醒等待的G,如果是则不需要唤醒,如果否则需要唤醒。
	// 1-是 0-否
	mutexWoken

	// 锁的饥饿状态标识位,最右侧第三个bit位
	// 1-饥饿模式 0-正常模式
	mutexStarving

	// 该常量不具有意义,仅仅表示state最右侧的3个bit位代表状态标识
	mutexWaiterShift = iota // 3

	// Mutex fairness.
	//
	// Mutex can be in 2 modes of operations: normal and starvation.
	// In normal mode waiters are queued in FIFO order, but a woken up waiter
	// does not own the mutex and competes with new arriving goroutines over
	// the ownership. New arriving goroutines have an advantage -- they are
	// already running on CPU and there can be lots of them, so a woken up
	// waiter has good chances of losing. In such case it is queued at front
	// of the wait queue. If a waiter fails to acquire the mutex for more than 1ms,
	// it switches mutex to the starvation mode.
	//
	// In starvation mode ownership of the mutex is directly handed off from
	// the unlocking goroutine to the waiter at the front of the queue.
	// New arriving goroutines don't try to acquire the mutex even if it appears
	// to be unlocked, and don't try to spin. Instead they queue themselves at
	// the tail of the wait queue.
	//
	// If a waiter receives ownership of the mutex and sees that either
	// (1) it is the last waiter in the queue, or (2) it waited for less than 1 ms,
	// it switches mutex back to normal operation mode.
	//
	// Normal mode has considerably better performance as a goroutine can acquire
	// a mutex several times in a row even if there are blocked waiters.
	// Starvation mode is important to prevent pathological cases of tail latency.
	// 正常模式转换为饥饿模式的 等待时间条件阈值
	starvationThresholdNs = 1e6
)

// Lock locks m.
// If the lock is already in use, the calling goroutine
// blocks until the mutex is available.
// Mutex 阻塞式加锁方法,如果锁正在被占用,则"阻塞"等待锁释放,直到获取到锁
func (m *Mutex) Lock() {
	// 1.尝试直接加锁
	// 通过CAS,尝试将state从0设置为1(mutexLocked) = ...001
	if atomic.CompareAndSwapInt32(&m.state, 0, mutexLocked) {
		if race.Enabled {
			race.Acquire(unsafe.Pointer(m))
		}
		// 只有state为初始化状态0 = (...0|0|0|0),所有bit位均为0才会成立
		// (阻塞队列等待数量 && 非饥饿模式 && 没有协程在抢锁 && 锁没有被持有) 只有此情况才会步入到这里,直接加锁成功并且返回。
		// 加锁成功,直接返回
		return
	}
	// 第一次加锁失败,使用自旋策略和阻塞策略获取锁
	m.lockSlow()
}

// TryLock tries to lock m and reports whether it succeeded.
//
// Note that while correct uses of TryLock do exist, they are rare,
// and use of TryLock is often a sign of a deeper problem
// in a particular use of mutexes.
func (m *Mutex) TryLock() bool {
	// 获取锁的状态信息
	old := m.state

	// 1. 如果当前锁已被持有,或者处于饥饿模式,直接返回false
	if old&(mutexLocked|mutexStarving) != 0 {
		return false
	}

	// There may be a goroutine waiting for the mutex, but we are
	// running now and can try to grab the mutex before that
	// goroutine wakes up.
	// 2. 通过CAS,将锁的状态标识由0改为1,如果失败直接返回false
	if !atomic.CompareAndSwapInt32(&m.state, old, old|mutexLocked) {
		return false
	}

	if race.Enabled {
		race.Acquire(unsafe.Pointer(m))
	}
	// 获取锁
	return true
}

// 第一次尝试加锁失败后,所执行的方法,也是加锁的核心逻辑
func (m *Mutex) lockSlow() {
	// 2. 通过自旋或者阻塞的方式进行加锁,直到获取锁。

	// 声明一个int64类型,用于记录当前G如果需要阻塞,那么阻塞的开始时间戳(纳秒)
	// 只有当前G要被阻塞并且挂起时初始化值
	// 用来计算当前G被阻塞了多长时间,然后决定锁的模式之间的转换
	// 并且使用该时间判断是否是被唤醒的G,如果是被唤醒后又要阻塞,则回退到阻塞队列头部
	var waitStartTime int64

	// 是否需要转换为饥饿模式，默认为false，当前G被阻塞过后又被唤醒后才会用到该属性。
	// 被唤醒后会根据waitStartTime计算一共阻塞了多长时间,如果大于1ms,此值会被值为true。
	starving := false

	// 当前是否有G在取锁,默认为false。
	awoke := false

	// 当前G自旋获取锁失败的次数
	iter := 0

	// 开始自旋或者阻塞之前,获取一次锁的状态,作为预期值.
	old := m.state
	// 循环不断加锁,直到获取到锁.
	for {
		// 2.1 自旋式加锁(乐观)处理
		// 判断是否进行自旋加锁，只有((锁已经被持有 && 不处于饥饿模式) && 还有可自旋次数)成立才会进行自旋
		if old&(mutexLocked|mutexStarving) == mutexLocked && runtime_canSpin(iter) {
			// 步入这里,表示锁已经被持有,并且不处于饥饿模式,但是还有可自旋次数

			// 如果当前没有G在抢锁,并且锁的阻塞队列中有G在等待唤醒。
			// 此时为了优化效率,将mutexWoken设置为1,表示有协程正在抢锁,就不要从队列中唤醒G了,直接让正在抢锁的新G获取。
			// 避免再有其他协程和自己抢锁。
			if !awoke && old&mutexWoken == 0 && old>>mutexWaiterShift != 0 && atomic.CompareAndSwapInt32(&m.state, old, old|mutexWoken) {
				// 将mutexWoken标识位设置为1
				// 表示当前存在协程正在获取锁
				awoke = true
			}
			// 进行自旋
			runtime_doSpin()
			// 自旋次数+1
			iter++
			// 再次获取锁的信息
			old = m.state
			// 继续下一次自旋获取锁
			continue
		}
		// 2.2 如果不满足自旋条件,或者自旋次数用尽后处理 (处于饥饿模式 || 没有可自选次数)

		// 再次获取一次加锁之前的state状态信息,作为加锁成功后的状态信息,如果失败此数据则会被无效丢弃。
		new := old
		// 判断锁是否已经是正常模式,如果是则直接加锁
		if old&mutexStarving == 0 {
			// 加锁,注意这里并非百分百加锁成功,需要后续的校验
			new |= mutexLocked
		}

		// 如果锁处于已上锁状态,或者当前锁是饥饿模式,那么就进入阻塞队列
		if old&(mutexLocked|mutexStarving) != 0 {
			// 阻塞等待的协程数量+1
			new += 1 << mutexWaiterShift
		}
		// 判断当前锁是否需要设置为饥饿模式,如果需要则设置。
		// 当一个G被唤醒之后,重新抢锁才会执行到这里.
		if starving && old&mutexLocked != 0 {
			// 设置为饥饿模式
			new |= mutexStarving
		}
		// 当前协程抢锁失败,即将要被挂起,所以需要将mutexWoken重置为0,表示没有协程在抢锁。
		if awoke {
			// The goroutine has been woken from sleep,
			// so we need to reset the flag in either case.
			if new&mutexWoken == 0 {
				throw("sync: inconsistent mutex state")
			}
			// 当前G被唤醒,无论是否抢到锁,我们都需要将mutexWoken恢复成0,表示当前没有协程在抢锁
			new &^= mutexWoken
		}
		// 2.3
		// 通过CAS,将加锁产生的新锁信息，更新到state中
		// 如果更新失败,则加锁失败
		// 注意,如果更新成功,并不一定是加锁成功,可能是在这过程中,并没有其他G进来抢锁,
		// 具体是否加锁成功,需要进一步判断
		if atomic.CompareAndSwapInt32(&m.state, old, new) {

			// 判断是否加锁成功
			// 如果锁状态标识为0(未加锁) 并且是正常模式则加锁成功。
			if old&(mutexLocked|mutexStarving) == 0 {
				// 加锁成功,退出循环
				break // locked the mutex with CAS
			}

			// 判断是否是从队列头部所唤醒的G,如果是G则放入阻塞队列的头部
			queueLifo := waitStartTime != 0
			// 判断当前G开始阻塞时间戳是否为0
			// 如果是0,则表示第一次循环
			if waitStartTime == 0 {
				// 获取当前时间纳秒,作为开始阻塞挂起的时间
				waitStartTime = runtime_nanotime()
			}
			// 将当前G添加到阻塞队列,当前G会一直阻塞到这一步,直到被唤醒...
			runtime_SemacquireMutex(&m.sema, queueLifo, 1)

			// 4.从阻塞队列被唤醒

			// 4.1 判断模式转换,计算当前G被阻塞了多长时间(当前时间戳 - 开始阻塞时的时间戳 > 1ms)
			//	   如果时长大于1ms就转换为饥饿模式,反之如果小于1ms,则转换为正常模式
			starving = starving || runtime_nanotime()-waitStartTime > starvationThresholdNs
			old = m.state

			// 4.2 被唤醒后,如果锁之前就是饥饿模式(公平模式),表示当前G会直接获取锁的持有权。
			if old&mutexStarving != 0 {
				// 当前G被从阻塞队列中唤醒,已经是饥饿模式,那也就意味着本次肯定会抢到锁
				if old&(mutexLocked|mutexWoken) != 0 || old>>mutexWaiterShift == 0 {
					throw("sync: inconsistent mutex state")
				}
				// 创建一个新的锁的状态信息
				// 并且将锁标识位设置为1,并且阻塞队列的G数量-1。
				delta := int32(mutexLocked - 1<<mutexWaiterShift)

				// 如果当前阻塞队列已经为空,或者阻塞时间小于1ms,则转换为正常模式
				if !starving || old>>mutexWaiterShift == 1 {
					// Exit starvation mode.
					// Critical to do it here and consider wait time.
					// Starvation mode is so inefficient, that two goroutines
					// can go lock-step infinitely once they switch mutex
					// to starvation mode.
					// 装换为正常模式
					delta -= mutexStarving
				}
				// 以相加的方式,更新state状态
				atomic.AddInt32(&m.state, delta)
				// 加锁成功 退出
				break
			}
			// 4.3 从队列中被唤醒,正常模式下,继续尝试抢锁、自旋、阻塞这个流程.

			awoke = true // 标识有协程正在抢锁
			iter = 0     // 重置自旋次数
		} else {
			// 加锁失败,重置
			old = m.state
		}
	}

	if race.Enabled {
		race.Acquire(unsafe.Pointer(m))
	}
}

// Unlock unlocks m.
// It is a run-time error if m is not locked on entry to Unlock.
//
// A locked Mutex is not associated with a particular goroutine.
// It is allowed for one goroutine to lock a Mutex and then
// arrange for another goroutine to unlock it.
// Mutex 解锁方法
func (m *Mutex) Unlock() {
	if race.Enabled {
		_ = m.state
		race.Release(unsafe.Pointer(m))
	}

	// Fast path: drop lock bit.
	// 1.直接通过原子操作,尝试解锁
	//   将state值-1,并且获取值。
	new := atomic.AddInt32(&m.state, -mutexLocked)

	// 3. 判断操作后的值是否为0,如果为0,表示解锁成功,直接返回即可。
	// 只有state为 1 == 0001 == (阻塞队列为空 && 非饥饿模式 && 锁已被抢占)才会直接解锁成功
	if new != 0 {
		// Outlined slow path to allow inlining the fast path.
		// To hide unlockSlow during tracing we skip one extra frame when tracing GoUnblock.
		// 尝试解锁失败,进一步处理
		m.unlockSlow(new)
	}
	// 2. 如果为0,则表示解锁之前的状态为 ...0001,所以可以直接成功
}

func (m *Mutex) unlockSlow(new int32) {
	// 3.1 校验锁是否是出于加锁状态,如果未加锁情况下进行释放锁,则panic
	if (new+mutexLocked)&mutexLocked == 0 {
		fatal("sync: unlock of unlocked mutex")
	}
	// 根据正常模式和饥饿模式进行不同解锁处理

	// 3.2 如果是正常模式
	if new&mutexStarving == 0 {
		old := new
		// 循环尝试
		for {
			// If there are no waiters or a goroutine has already
			// been woken or grabbed the lock, no need to wake anyone.
			// In starvation mode ownership is directly handed off from unlocking
			// goroutine to the next waiter. We are not part of this chain,
			// since we did not observe mutexStarving when we unlocked the mutex above.
			// So get off the way.

			// 3.2.2 判断解锁完成后是否需要从阻塞队列中唤醒阻塞的协程,当满足如下条件之一时,则不需要唤醒
			// (1) 阻塞队列为空。没有阻塞等待的协程,当然不需要唤醒。
			// (2) 锁被释放之后又被其他协程占有。已经被持有了,所以也没必要唤醒阻塞的协程了。
			// (3) 当前有正在自旋尝试获取锁的协程。已经有协程再尝试获取锁,也没必要唤醒阻塞的协程,避免没有意义的竞争。
			// (4)
			//
			if old>>mutexWaiterShift == 0 || old&(mutexLocked|mutexWoken|mutexStarving) != 0 {
				return
			}
			// Grab the right to wake someone.
			// 将阻塞队列中的数量-1,并且将锁标识为当前有协程正在抢夺锁.
			new = (old - 1<<mutexWaiterShift) | mutexWoken
			// 更新锁的状态
			if atomic.CompareAndSwapInt32(&m.state, old, new) {
				// 更新成功,从队列头部唤醒等待的协程
				runtime_Semrelease(&m.sema, false, 1)
				return
			}
			// 更新失败,由其他协程介入,更新一下锁的信息,继续尝试唤醒
			old = m.state
		}
	} else {

		// Starving mode: handoff mutex ownership to the next waiter, and yield
		// our time slice so that the next waiter can start to run immediately.
		// Note: mutexLocked is not set, the waiter will set it after wakeup.
		// But mutex is still considered locked if mutexStarving is set,
		// so new coming goroutines won't acquire it.
		// 3.3 饥饿模式
		// 如果是饥饿模式,直接从阻塞队列头部唤醒协程即可。
		runtime_Semrelease(&m.sema, true, 1)
	}
}
