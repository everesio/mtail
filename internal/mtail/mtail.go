// Copyright 2011 Google Inc. All Rights Reserved.
// This file is available under the Apache license.

package mtail

import (
	"context"
	"encoding/json"
	"expvar"
	"fmt"
	"html/template"
	"io"
	"net"
	"net/http"
	"net/http/pprof"
	"os"
	"os/signal"
	"runtime"
	"sync"
	"syscall"
	"time"

	"github.com/golang/glog"
	"github.com/google/mtail/internal/exporter"
	"github.com/google/mtail/internal/logline"
	"github.com/google/mtail/internal/metrics"
	"github.com/google/mtail/internal/tailer"
	"github.com/google/mtail/internal/vm"
	"github.com/google/mtail/internal/watcher"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/common/version"
)

type BuildInfo struct {
	Branch   string
	Version  string
	Revision string
}

func (b BuildInfo) String() string {
	return fmt.Sprintf(
		"mtail version %s git revision %s go version %s go arch %s go os %s",
		b.Version,
		b.Revision,
		runtime.Version(),
		runtime.GOARCH,
		runtime.GOOS,
	)
}

// Server contains the state of the main mtail program.
type Server struct {
	lines chan *logline.LogLine // Channel of lines from tailer to VM engine.
	store *metrics.Store        // Metrics storage.
	w     watcher.Watcher

	t *tailer.Tailer     // t tails the watched files and feeds lines to the VMs.
	l *vm.Loader         // l loads programs and manages the VM lifecycle.
	e *exporter.Exporter // e manages the export of metrics from the store.

	reg *prometheus.Registry

	h        *http.Server
	listener net.Listener

	webquit   chan struct{} // Channel to signal shutdown from web UI.
	closeQuit chan struct{} // Channel to signal shutdown from code.
	closeOnce sync.Once     // Ensure shutdown happens only once.

	bindAddress     string    // address to bind HTTP server
	buildInfo       BuildInfo // go build information
	programPath     string    // path to programs to load
	logPathPatterns []string  // list of patterns to watch for log files to tail

	oneShot      bool // if set, mtail reads log files from the beginning, once, then exits
	compileOnly  bool // if set, mtail compiles programs then exits
	dumpAst      bool // if set, mtail prints the program syntax tree after parse
	dumpAstTypes bool // if set, mtail prints the program syntax tree after type checking
	dumpBytecode bool // if set, mtail prints the program bytecode after code generation

	overrideLocation            *time.Location // Timezone location to use when parsing timestamps
	expiredMetricGcTickInterval time.Duration  // Interval between expired metric removal runs
	staleLogGcTickInterval      time.Duration  // Interval between stale log gc runs
	syslogUseCurrentYear        bool           // if set, use the current year for timestamps that have no year information
	omitMetricSource            bool           // if set, do not link the source program to a metric
	omitProgLabel               bool           // if set, do not put the program name in the metric labels
	emitMetricTimestamp         bool           // if set, emit the metric's recorded timestamp
}

// StartTailing adds each log path pattern to the tailer.
func (m *Server) StartTailing() error {
	var err error
	for _, pattern := range m.logPathPatterns {
		glog.V(1).Infof("Tail pattern %q", pattern)
		if err = m.t.TailPattern(pattern); err != nil {
			glog.Warning(err)
		}
	}
	return nil
}

// initLoader constructs a new program loader and performs the initial load of program files in the program directory.
func (m *Server) initLoader() error {
	opts := []func(*vm.Loader) error{}
	if m.compileOnly {
		opts = append(opts, vm.CompileOnly)
		if m.oneShot {
			opts = append(opts, vm.ErrorsAbort)
		}
	}
	if m.dumpAst {
		opts = append(opts, vm.DumpAst)
	}
	if m.dumpAstTypes {
		opts = append(opts, vm.DumpAstTypes)
	}
	if m.dumpBytecode {
		opts = append(opts, vm.DumpBytecode)
	}
	if m.syslogUseCurrentYear {
		opts = append(opts, vm.SyslogUseCurrentYear)
	}
	if m.omitMetricSource {
		opts = append(opts, vm.OmitMetricSource)
	}
	if m.overrideLocation != nil {
		opts = append(opts, vm.OverrideLocation(m.overrideLocation))
	}
	var err error
	m.l, err = vm.NewLoader(m.programPath, m.store, m.lines, m.w, opts...)
	if err != nil {
		return err
	}
	if m.programPath == "" {
		return nil
	}
	if errs := m.l.LoadAllPrograms(); errs != nil {
		return errors.Errorf("Compile encountered errors:\n%s", errs)
	}
	return nil
}

// initExporter sets up an Exporter for this Server.
func (m *Server) initExporter() (err error) {
	opts := []func(*exporter.Exporter) error{}
	if m.omitProgLabel {
		opts = append(opts, exporter.OmitProgLabel)
	}
	if m.emitMetricTimestamp {
		opts = append(opts, exporter.EmitTimestamp)
	}
	m.e, err = exporter.New(m.store, opts...)
	if err != nil {
		return err
	}

	expvarDescs := map[string]*prometheus.Desc{
		// internal/tailer/file.go
		"log_errors_total":    prometheus.NewDesc("log_errors_total", "number of IO errors encountered per log file", []string{"logfile"}, nil),
		"log_rotations_total": prometheus.NewDesc("log_rotations_total", "number of log rotation events per log file", []string{"logfile"}, nil),
		"log_truncates_total": prometheus.NewDesc("log_truncates_total", "number of log truncation events log file", []string{"logfile"}, nil),
		"log_lines_total":     prometheus.NewDesc("log_lines_total", "number of lines read per log file", []string{"logfile"}, nil),
		// internal/vm/loader.go
		"line_count":          prometheus.NewDesc("line_count", "number of lines received by the program loader", nil, nil),
		"prog_loads_total":    prometheus.NewDesc("prog_loads_total", "number of program load events by program source filename", []string{"prog"}, nil),
		"prog_load_errors":    prometheus.NewDesc("prog_load_errors", "number of errors encountered when loading per program source filename", []string{"prog"}, nil),
		"prog_runtime_errors": prometheus.NewDesc("prog_runtime_errors", "number of errors encountered when executing programs per source filename", []string{"prog"}, nil),
		// internal/watcher/log_watcher.go
		"log_watcher_error_count": prometheus.NewDesc("log_watcher_error_count", "number of errors received from fsnotify", nil, nil),
	}
	// Using a non-pedantic registry means we can be looser with metrics that
	// are not fully specified at startup.
	m.reg = prometheus.NewRegistry()
	m.reg.MustRegister(m.e,
		prometheus.NewGoCollector(),
		prometheus.NewProcessCollector(prometheus.ProcessCollectorOpts{}))
	// Prefix all expvar metrics with 'mtail_'
	prometheus.WrapRegistererWithPrefix("mtail_", m.reg).MustRegister(
		prometheus.NewExpvarCollector(expvarDescs))

	// Create mtail_build_info metric.
	version.Branch = m.buildInfo.Branch
	version.Version = m.buildInfo.Version
	version.Revision = m.buildInfo.Revision
	m.reg.MustRegister(version.NewCollector("mtail"))
	return nil
}

// initTailer sets up a Tailer for this Server.
func (m *Server) initTailer() (err error) {
	opts := []func(*tailer.Tailer) error{}
	if m.oneShot {
		opts = append(opts, tailer.OneShot)
	}
	m.t, err = tailer.New(m.lines, m.w, opts...)
	return
}

const statusTemplate = `
<html>
<head>
<title>mtail on {{.BindAddress}}</title>
</head>
<body>
<h1>mtail on {{.BindAddress}}</h1>
<p>Build: {{.BuildInfo}}</p>
<p>Metrics: <a href="/json">json</a>, <a href="/metrics">prometheus</a>, <a href="/varz">varz</a></p>
<p>Debug: <a href="/debug/pprof">debug/pprof</a>, <a href="/debug/vars">debug/vars</a></p>
`

func (m *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	t, err := template.New("status").Parse(statusTemplate)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	data := struct {
		BindAddress string
		BuildInfo   string
	}{
		m.bindAddress,
		m.buildInfo.String(),
	}
	w.Header().Add("Content-type", "text/html")
	w.WriteHeader(http.StatusOK)
	if err = t.Execute(w, data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
	err = m.l.WriteStatusHTML(w)
	if err != nil {
		glog.Warningf("Error while writing loader status: %s", err)
	}
	err = m.t.WriteStatusHTML(w)
	if err != nil {
		glog.Warningf("Error while writing tailer status: %s", err)
	}
}

// New creates a MtailServer from the supplied Options.
func New(store *metrics.Store, w watcher.Watcher, options ...func(*Server) error) (*Server, error) {
	m := &Server{
		store:     store,
		lines:     make(chan *logline.LogLine),
		w:         w,
		webquit:   make(chan struct{}),
		closeQuit: make(chan struct{}),
		h:         &http.Server{},
	}
	if err := m.SetOption(options...); err != nil {
		return nil, err
	}
	if err := m.initExporter(); err != nil {
		return nil, err
	}
	if err := m.initLoader(); err != nil {
		return nil, err
	}
	if err := m.initTailer(); err != nil {
		return nil, err
	}
	return m, nil
}

// SetOption takes one or more option functions and applies them in order to MtailServer.
func (m *Server) SetOption(options ...func(*Server) error) error {
	for _, option := range options {
		if err := option(m); err != nil {
			return err
		}
	}
	return nil
}

// WriteMetrics dumps the current state of the metrics store in JSON format to
// the io.Writer.
func (m *Server) WriteMetrics(w io.Writer) error {
	m.store.RLock()
	b, err := json.MarshalIndent(m.store.Metrics, "", "  ")
	m.store.RUnlock()
	if err != nil {
		return errors.Wrap(err, "failed to marshal metrics into json")
	}
	_, err = w.Write(b)
	return err
}

// Serve begins the webserver and awaits a shutdown instruction.
func (m *Server) Serve() error {
	if m.bindAddress == "" {
		return errors.Errorf("No bind address provided.")
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/favicon.ico", FaviconHandler)
	mux.Handle("/", m)
	mux.HandleFunc("/json", http.HandlerFunc(m.e.HandleJSON))
	mux.Handle("/metrics", promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{}))
	mux.HandleFunc("/varz", http.HandlerFunc(m.e.HandleVarz))
	mux.HandleFunc("/quitquitquit", http.HandlerFunc(m.handleQuit))
	mux.Handle("/debug/vars", expvar.Handler())
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	m.h.Handler = mux
	m.e.StartMetricPush()

	errc := make(chan error, 1)
	go func() {
		glog.Infof("Listening on %s", m.listener.Addr())
		err := m.h.Serve(m.listener)

		if err == http.ErrServerClosed {
			err = nil
		}
		errc <- err
	}()
	m.WaitForShutdown()
	return <-errc
}

func (m *Server) handleQuit(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		w.Header().Add("Allow", "POST")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	fmt.Fprintf(w, "Exiting...")
	close(m.webquit)
}

// WaitForShutdown handles shutdown requests from the system or the UI.
func (m *Server) WaitForShutdown() {
	n := make(chan os.Signal, 1)
	signal.Notify(n, os.Interrupt, syscall.SIGTERM)
	select {
	case <-n:
		glog.Info("Received SIGTERM, exiting...")
	case <-m.webquit:
		glog.Info("Received Quit from HTTP, exiting...")
	case <-m.closeQuit:
		glog.Info("Received quit internally, exiting...")
	}
	if err := m.Close(); err != nil {
		glog.Warning(err)
	}
}

// Close handles the graceful shutdown of this mtail instance, ensuring that it only occurs once.
func (m *Server) Close() error {
	m.closeOnce.Do(func() {
		glog.Info("Shutdown requested.")
		close(m.closeQuit)
		// If we have a tailer (i.e. not in test) then signal the tailer to
		// shut down, which will cause the watcher to shut down and for the
		// lines channel to close, causing the loader to start shutdown.
		if m.t != nil {
			err := m.t.Close()
			if err != nil {
				glog.Infof("tailer close failed: %s", err)
			}
		} else {
			// Without a tailer, MtailServer has ownership of the lines channel.
			glog.V(2).Info("No tailer, closing lines channel directly.")
			close(m.lines)
		}
		// If we have a loader, wait for it to signal that it has completed shutdown.
		if m.l != nil {
			<-m.l.VMsDone
		} else {
			glog.V(2).Info("No loader, so not waiting for loader shutdown.")
		}
		if m.h != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			if err := m.h.Shutdown(ctx); err != nil {
				glog.Error(err)
			}
			cancel()
		}
		glog.Info("END OF LINE")
	})
	return nil
}

// Run starts MtailServer's primary function, in which it watches the log
// files for changes and sends any new lines found into the lines channel for
// pick up by the virtual machines. If OneShot mode is enabled, it will exit.
func (m *Server) Run() error {
	if m.compileOnly {
		glog.Info("compile-only is set, exiting")
		return nil
	}
	if err := m.StartTailing(); err != nil {
		glog.Exitf("tailing failed: %s", err)
	}
	if m.oneShot {
		err := m.Close()
		if err != nil {
			return err
		}
		fmt.Printf("Metrics store:")
		if err := m.WriteMetrics(os.Stdout); err != nil {
			return err
		}
	} else {
		m.store.StartGcLoop(m.expiredMetricGcTickInterval)
		m.t.StartGcLoop(m.staleLogGcTickInterval)
		if err := m.Serve(); err != nil {
			return err
		}
	}
	return nil
}

func FaviconHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "image/x-icon")
	w.Header().Set("Cache-Control", "public, max-age=7776000")
	if _, err := w.Write(logoFavicon); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (m *Server) Addr() string {
	if m.listener == nil {
		return "none"
	}
	return m.listener.Addr().String()
}
