package agent

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"math/rand"
	"net/http"
	"net/url"
	"runtime/pprof"
	"runtime/trace"
	"strings"
	"sync"
	"time"

	"github.com/profefe/profefe/pkg/profile"
)

const (
	defaultProfileType = profile.TypeCPU

	defaultDuration     = 10 * time.Second
	defaultTickInterval = time.Minute

	backoffMinDelay    = 5 * time.Second
	backoffMaxDelay    = 2 * time.Minute
	backoffMaxAttempts = 10
)

func init() {
	rand.Seed(time.Now().UnixNano())
}

func Start(addr, service string, opts ...Option) (*Agent, error) {
	agent := New(addr, service, opts...)
	if err := agent.Start(context.Background()); err != nil {
		return nil, err
	}
	return agent, nil
}

type httpClient interface {
	Do(req *http.Request) (*http.Response, error)
}

type Agent struct {
	CPUProfile          bool
	CPUProfileDuration  time.Duration
	HeapProfile         bool
	BlockProfile        bool
	MutexProfile        bool
	GoroutineProfile    bool
	ThreadcreateProfile bool
	Trace               bool
	TraceDuration       time.Duration

	service   string
	rawLabels strings.Builder

	logf func(format string, v ...interface{})

	rawClient     httpClient
	collectorAddr string

	tick time.Duration
	stop chan struct{} // signals the beginning of stop
	done chan struct{} // closed when stopping is done
}

func New(addr, service string, opts ...Option) *Agent {
	a := &Agent{
		CPUProfile:         true, // enable CPU profiling by default
		CPUProfileDuration: defaultDuration,

		collectorAddr: addr,
		service:       service,

		rawClient: http.DefaultClient,
		logf:      func(format string, v ...interface{}) {},

		tick: defaultTickInterval,
		stop: make(chan struct{}),
		done: make(chan struct{}),
	}

	for _, opt := range opts {
		opt(a)
	}

	return a
}

func (a *Agent) Start(ctx context.Context) error {
	if a.collectorAddr == "" {
		return fmt.Errorf("failed to start agent: collector address is empty")
	}

	go a.collectAndSend(ctx)

	return nil
}

func (a *Agent) Stop() error {
	close(a.stop)
	<-a.done
	return nil
}

func (a *Agent) collectProfile(ctx context.Context, ptype profile.ProfileType, buf *bytes.Buffer) error {
	switch ptype {
	case profile.TypeCPU:
		err := pprof.StartCPUProfile(buf)
		if err != nil {
			return fmt.Errorf("failed to start CPU profile: %v", err)
		}
		sleep(a.CPUProfileDuration, ctx.Done())
		pprof.StopCPUProfile()
	case profile.TypeHeap:
		err := pprof.WriteHeapProfile(buf)
		if err != nil {
			return fmt.Errorf("failed to write heap profile: %v", err)
		}
	case profile.TypeBlock,
		profile.TypeMutex,
		profile.TypeGoroutine,
		profile.TypeThreadcreate:

		p := pprof.Lookup(ptype.String())
		if p == nil {
			return fmt.Errorf("unknown profile type %v", ptype)
		}
		err := p.WriteTo(buf, 0)
		if err != nil {
			return fmt.Errorf("failed to write %s profile: %v", ptype, err)
		}
	case profile.TypeTrace:
		err := trace.Start(buf)
		if err != nil {
			return fmt.Errorf("failed to start trace: %v", err)
		}
		sleep(a.TraceDuration, ctx.Done())
		trace.Stop()
	default:
		return fmt.Errorf("unknown profile type %v", ptype)
	}

	return nil
}

func (a *Agent) sendProfile(ctx context.Context, ptype profile.ProfileType, createdAt time.Time, buf *bytes.Buffer) error {
	q := url.Values{}
	q.Set("service", a.service)
	q.Set("labels", a.rawLabels.String())
	q.Set("type", ptype.String())

	// Set create time for trace
	if ptype == profile.TypeTrace {
		q.Set("created_at", createdAt.Format("2006-01-02T15:04:05"))
	}

	surl := a.collectorAddr + "/api/0/profiles?" + q.Encode()
	req, err := http.NewRequest(http.MethodPost, surl, buf)
	if err != nil {
		return err
	}
	req = req.WithContext(ctx)

	return DoRetryAttempts(
		backoffMinDelay,
		backoffMaxDelay,
		backoffMaxAttempts,
		func() error { return a.doRequest(req, nil) },
	)
}

func (a *Agent) doRequest(req *http.Request, v io.Writer) error {
	resp, err := a.rawClient.Do(req)
	if err, ok := err.(*url.Error); ok && err.Err == context.Canceled {
		return Cancel(err)
	}
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		respBody, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			return fmt.Errorf("unexpected respose %s: %v", resp.Status, err)
		}
		if resp.StatusCode >= 500 {
			return fmt.Errorf("unexpected respose from collector %s: %s", resp.Status, respBody)
		}
		return Cancel(fmt.Errorf("bad request: collector responded with %s: %s", resp.Status, respBody))
	}

	if v != nil {
		_, err := io.Copy(v, resp.Body)
		return err
	}

	return nil
}

func (a *Agent) collectAndSend(ctx context.Context) {
	defer close(a.done)

	ctx, cancel := context.WithCancel(ctx)
	go func() {
		<-a.stop
		cancel()
	}()

	var (
		ptype = a.nextProfileType(profile.TypeUnknown)
		timer = time.NewTimer(tickInterval(0))

		buf bytes.Buffer
	)

	for {
		select {
		case <-a.stop:
			if !timer.Stop() {
				<-timer.C
			}
			return
		case <-timer.C:
			createdAt := time.Now().UTC()
			if err := a.collectProfile(ctx, ptype, &buf); err != nil {
				a.logf("[FAIL] unable to collect profiles: %v", err)
			} else {
				// XXX WANDA add debug
				a.logf(" going to send type %v len is %d", ptype, buf.Len())
				if err := a.sendProfile(ctx, ptype, createdAt, &buf); err != nil {
					a.logf("[FAIL] unable to send profiles: %v", err)
				}
			}

			buf.Reset()

			ptype = a.nextProfileType(ptype)

			var tick time.Duration
			if ptype == defaultProfileType {
				// we took the full set of profiles, sleep for the whole tick
				tick = a.tick
			}

			timer.Reset(tickInterval(tick))
		}
	}
}

func (a *Agent) nextProfileType(ptype profile.ProfileType) profile.ProfileType {
	// special case to choose initial profile type on the first call
	if ptype == profile.TypeUnknown {
		return defaultProfileType
	}

	for {
		switch ptype {
		case profile.TypeCPU:
			ptype = profile.TypeHeap
			if a.HeapProfile {
				return ptype
			}
		case profile.TypeHeap:
			ptype = profile.TypeBlock
			if a.BlockProfile {
				return ptype
			}
		case profile.TypeBlock:
			ptype = profile.TypeMutex
			if a.MutexProfile {
				return ptype
			}
		case profile.TypeMutex:
			ptype = profile.TypeGoroutine
			if a.GoroutineProfile {
				return ptype
			}
		case profile.TypeGoroutine:
			ptype = profile.TypeThreadcreate
			if a.ThreadcreateProfile {
				return ptype
			}
		case profile.TypeThreadcreate:
			ptype = profile.TypeTrace
			if a.Trace {
				return ptype
			}
		case profile.TypeTrace:
			ptype = profile.TypeCPU
			if a.CPUProfile {
				return ptype
			}
		}

	}
}

func tickInterval(d time.Duration) time.Duration {
	// add up to extra 10 seconds to sleep to dis-align profiles of different instances
	noise := time.Second + time.Duration(rand.Intn(9))*time.Second
	return d + noise
}

var timersPool = sync.Pool{}

func sleep(d time.Duration, cancel <-chan struct{}) {
	timer, _ := timersPool.Get().(*time.Timer)
	if timer == nil {
		timer = time.NewTimer(d)
	} else {
		timer.Reset(d)
	}

	select {
	case <-timer.C:
	case <-cancel:
		if !timer.Stop() {
			<-timer.C
		}
	}

	timersPool.Put(timer)
}
