package common

import (
	"sync/atomic"

	"github.com/sirupsen/logrus"
)

type Service interface {
	Start() (bool, error)
	OnStart() error

	Stop() bool
	OnStop()

	Reset() (bool, error)
	OnReset() error

	IsRunning() bool

	String() string
}

/*
Classical-inheritance-style service declarations. Services can be started, then
stopped, then optionally restarted.

Users can override the OnStart/OnStop methods. In the absence of errors, these
methods are guaranteed to be called at most once. If OnStart returns an error,
service won't be marked as started, so the user can call Start again.

A call to Reset will panic, unless OnReset is overwritten, allowing
OnStart/OnStop to be called again.

The caller must ensure that Start and Stop are not called concurrently.

It is ok to call Stop without calling Start first.

Typical usage:

	type FooService struct {
		BaseService
		// private fields
	}

	func NewFooService() *FooService {
		fs := &FooService{
			// init
		}
		fs.BaseService = *NewBaseService(logger, "FooService", fs)
		return fs
	}

	func (fs *FooService) OnStart() error {
		fs.BaseService.OnStart() // Always call the overridden method.
		// initialize private fields
		// start subroutines, etc.
	}

	func (fs *FooService) OnStop() error {
		fs.BaseService.OnStop() // Always call the overridden method.
		// close/destroy private fields
		// stop subroutines, etc.
	}
*/

type BaseService struct {
	logger  *logrus.Logger
	name    string
	started uint32 // atomic
	stopped uint32 // atomic
	Quit    chan struct{}

	// The "subclass" of BaseService
	impl Service
}

func NewBaseService(logger *logrus.Logger, name string, impl Service) *BaseService {
	return &BaseService{
		logger: logger,
		name:   name,
		Quit:   make(chan struct{}),
		impl:   impl,
	}
}

// Implements Servce
func (bs *BaseService) Start() (bool, error) {
	if atomic.CompareAndSwapUint32(&bs.started, 0, 1) {
		if atomic.LoadUint32(&bs.stopped) == 1 {
			if bs.logger != nil {
				bs.logger.Warn("Not starting ", bs.name, " -- already stopped, impl:", bs.impl)
			}
			return false, nil
		} else {
			if bs.logger != nil {
				bs.logger.Info("Starting ", bs.name, " impl:", bs.impl)
			}
		}
		err := bs.impl.OnStart()
		if err != nil {
			// revert flag
			atomic.StoreUint32(&bs.started, 0)
			return false, err
		}
		return true, err
	} else {
		if bs.logger != nil {
			bs.logger.Debug("Not starting ", bs.name, " -- already started, impl:", bs.impl)
		}
		return false, nil
	}
}

// Implements Service
// NOTE: Do not put anything in here,
// that way users don't need to call BaseService.OnStart()
func (bs *BaseService) OnStart() error { return nil }

// Implements Service
func (bs *BaseService) Stop() bool {
	if atomic.CompareAndSwapUint32(&bs.stopped, 0, 1) {
		if bs.logger != nil {
			bs.logger.Info("Stopping ", bs.name, " ,impl:", bs.impl)
		}
		bs.impl.OnStop()
		close(bs.Quit)
		return true
	} else {
		if bs.logger != nil {
			bs.logger.Debug("Stopping ", bs.name, " (ignoring: already stopped) , impl:", bs.impl)
		}
		return false
	}
}

// Implements Service
// NOTE: Do not put anything in here,
// that way users don't need to call BaseService.OnStop()
func (bs *BaseService) OnStop() {}

// Implements Service
func (bs *BaseService) Reset() (bool, error) {
	if atomic.CompareAndSwapUint32(&bs.stopped, 1, 0) {
		// whether or not we've started, we can reset
		atomic.CompareAndSwapUint32(&bs.started, 1, 0)

		bs.Quit = make(chan struct{})
		return true, bs.impl.OnReset()
	} else {
		if bs.logger != nil {
			bs.logger.Debug("Can't reset ", bs.name, ". Not stopped, impl:", bs.impl)
		}
		return false, nil
	}
	// never happens
	return false, nil
}

// Implements Service
func (bs *BaseService) OnReset() error {
	PanicSanity("The service cannot be reset")
	return nil
}

// Implements Service
func (bs *BaseService) IsRunning() bool {
	return atomic.LoadUint32(&bs.started) == 1 && atomic.LoadUint32(&bs.stopped) == 0
}

func (bs *BaseService) Wait() {
	<-bs.Quit
}

// Implements Servce
func (bs *BaseService) String() string {
	return bs.name
}

//----------------------------------------

type QuitService struct {
	BaseService
}

func NewQuitService(logger *logrus.Logger, name string, impl Service) *QuitService {
	if logger != nil {
		logger.Warn("QuitService is deprecated, use BaseService instead")
	}
	return &QuitService{
		BaseService: *NewBaseService(logger, name, impl),
	}
}
