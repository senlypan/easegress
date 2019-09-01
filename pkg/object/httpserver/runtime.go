package httpserver

import (
	"fmt"
	"net/http"
	"reflect"
	"sync"
	"sync/atomic"
	"time"

	"github.com/megaease/easegateway/pkg/context"
	"github.com/megaease/easegateway/pkg/graceupdate"
	"github.com/megaease/easegateway/pkg/logger"
	"github.com/megaease/easegateway/pkg/util/httpstat"
	"github.com/megaease/easegateway/pkg/util/topn"

	"golang.org/x/net/netutil"
)

const (
	defaultKeepAliveTimeout = 60 * time.Second

	checkFailedTimeout = 10 * time.Second

	stateNil     stateType = "nil"
	stateFailed            = "failed"
	stateRunning           = "running"
	stateClosed            = "closed"

	topNum = 10
)

var (
	errNil = fmt.Errorf("")
	gnet   = graceupdate.Global
)

type (
	stateType string

	eventStart       struct{}
	eventCheckFailed struct{}
	eventServeFailed struct {
		startNum uint64
		err      error
	}
	eventReload struct{ nextSpec *Spec }
	eventClose  struct{ done chan struct{} }

	runtime struct {
		handlers  *sync.Map
		spec      *Spec
		server    *http.Server
		mux       *mux
		startNum  uint64
		eventChan chan interface{}

		// status
		state atomic.Value // stateType
		err   atomic.Value // error

		httpStat *httpstat.HTTPStat
		topN     *topn.TopN
	}

	// Status contains all status gernerated by runtime, for displaying to users.
	Status struct {
		Timestamp uint64 `yaml:"timestamp"`

		State stateType `yaml:"state"`
		Error string    `yaml:"error,omitempty"`

		*httpstat.Status
		TopN *topn.Status `yaml:"topN"`
	}

	// Handler is handler handling HTTPContext.
	Handler interface {
		Handle(context.HTTPContext)
	}
)

func newRuntime(handlers *sync.Map) *runtime {
	r := &runtime{
		handlers:  handlers,
		eventChan: make(chan interface{}, 10),
		httpStat:  httpstat.New(),
		topN:      topn.New(topNum),
	}

	r.mux = newMux(r.handlers, r.httpStat, r.topN)

	r.setState(stateNil)
	r.setError(errNil)

	go r.fsm()
	go r.checkFailed()

	return r
}

// Close closes runtime.
func (r *runtime) Close() {
	done := make(chan struct{})
	r.eventChan <- &eventClose{done: done}
	<-done
}

// Status returns HTTPServer Status.
func (r *runtime) Status() *Status {
	return &Status{
		State:  r.getState(),
		Error:  r.getError().Error(),
		Status: r.httpStat.Status(),
		TopN:   r.topN.Status(),
	}
}

// FSM is the finite-state-machine for the runtime.
func (r *runtime) fsm() {
	for e := range r.eventChan {
		switch e := e.(type) {
		case *eventStart:
			r.handleEventStart(e)
		case *eventCheckFailed:
			r.handleEventCheckFailed(e)
		case *eventServeFailed:
			r.handleEventServeFailed(e)
		case *eventReload:
			r.handleEventReload(e)
		case *eventClose:
			r.handleEventClose(e)
			// NOTE: We don't close hs.eventChan,
			// in case of panic of any other goroutines
			// to send event to it later.
			return
		default:
			logger.Errorf("BUG: unknown event: %T\n", e)
		}
	}
}

func (r *runtime) handleEventStart(e *eventStart) {
	r.startServer()
}

func (r *runtime) reload(nextSpec *Spec) {
	if nextSpec != nil {
		r.mux.reloadRules(nextSpec)
	}

	// NOTE: Due to the mechanism of scheduler,
	// nextSpec must not be nil, just defensive programming here.
	switch {
	case r.spec == nil && nextSpec == nil:
		logger.Errorf("BUG: nextSpec is nil")
		// Nothing to do.
	case r.spec == nil && nextSpec != nil:
		r.spec = nextSpec
		r.startServer()
	case r.spec != nil && nextSpec == nil:
		logger.Errorf("BUG: nextSpec is nil")
		r.spec = nil
		r.closeServer()
	case r.spec != nil && nextSpec != nil:
		if r.needRestartServer(nextSpec) {
			r.spec = nextSpec
			r.closeServer()
			r.startServer()
		} else {
			r.spec = nextSpec
		}
	}
}

func (r *runtime) setState(state stateType) {
	r.state.Store(state)
}

func (r *runtime) getState() stateType {
	return r.state.Load().(stateType)
}

func (r *runtime) setError(err error) {
	if err == nil {
		r.err.Store(errNil)
	} else {
		// NOTE: For type safe.
		r.err.Store(fmt.Errorf("%v", err))
	}
}

func (r *runtime) getError() error {
	err := r.err.Load()
	if err == nil {
		return nil
	}
	return err.(error)
}

func (r *runtime) needRestartServer(nextSpec *Spec) bool {
	x := *r.spec
	y := *nextSpec
	x.Rules, y.Rules = nil, nil

	// The update of rules need not to shutdown server.
	return !reflect.DeepEqual(x, y)
}

func (r *runtime) startServer() {
	keepAliveTimeout := defaultKeepAliveTimeout
	if r.spec.KeepAliveTimeout != "" {
		t, err := time.ParseDuration(r.spec.KeepAliveTimeout)
		if err != nil {
			logger.Errorf("BUG: parse duration %s failed: %v",
				r.spec.KeepAliveTimeout, err)
		} else {
			keepAliveTimeout = t
		}
	}

	listener, err := gnet.Listen("tcp", fmt.Sprintf(":%d", r.spec.Port))
	if err != nil {
		r.setState(stateFailed)
		r.setError(err)

		return
	}

	limitListener := netutil.LimitListener(listener, int(r.spec.MaxConnections))

	srv := &http.Server{
		Addr:        fmt.Sprintf(":%d", r.spec.Port),
		Handler:     r.mux,
		IdleTimeout: keepAliveTimeout,
	}
	srv.SetKeepAlivesEnabled(r.spec.KeepAlive)

	if r.spec.HTTPS {
		tlsConfig, _ := r.spec.tlsConfig()
		srv.TLSConfig = tlsConfig
	}

	r.server = srv
	r.startNum++
	r.setState(stateRunning)
	r.setError(nil)

	go func(https bool, startNum uint64) {
		var err error
		if https {
			err = r.server.ServeTLS(limitListener, "", "")
		} else {
			err = r.server.Serve(limitListener)
		}
		if err != http.ErrServerClosed {
			r.eventChan <- &eventServeFailed{
				err:      err,
				startNum: startNum,
			}
		}
	}(r.spec.HTTPS, r.startNum)
}

func (r *runtime) closeServer() {
	if r.server == nil {
		return
	}
	// NOTE: It's safe to shutdown serve failed server.
	ctx, cancelFunc := serverShutdownContext()
	defer cancelFunc()
	err := r.server.Shutdown(ctx)
	if err != nil {
		logger.Warnf("shutdown httpserver %s failed: %v",
			r.spec.Name, err)
	}
}

func (r *runtime) checkFailed() {
	ticker := time.NewTicker(checkFailedTimeout)
	for range ticker.C {
		state := r.getState()
		if state == stateFailed {
			r.eventChan <- &eventCheckFailed{}
		} else if state == stateClosed {
			ticker.Stop()
			return
		}
	}
}

func (r *runtime) handleEventCheckFailed(e *eventCheckFailed) {
	if r.getState() == stateFailed {
		r.startServer()
	}
}

func (r *runtime) handleEventServeFailed(e *eventServeFailed) {
	if r.startNum > e.startNum {
		return
	}
	r.setState(stateFailed)
	r.setError(e.err)
}

func (r *runtime) handleEventReload(e *eventReload) {
	r.reload(e.nextSpec)
}

func (r *runtime) handleEventClose(e *eventClose) {
	r.closeServer()
	close(e.done)
}