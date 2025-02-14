package app

import (
	"context"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"

	"github.com/diamondburned/gotk4/pkg/gio/v2"
	"github.com/diamondburned/gotk4/pkg/glib/v2"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
	"github.com/diamondburned/gotkit/components/errpopup"
	"github.com/diamondburned/gotkit/gtkutil"
	"github.com/diamondburned/gotkit/gtkutil/cssutil"

	coreglib "github.com/diamondburned/gotk4/pkg/core/glib"
)

var isDevel bool

func init() {
	build, ok := debug.ReadBuildInfo()
	if !ok {
		return
	}

	find := func(k string) string {
		for _, s := range build.Settings {
			if s.Key == k {
				return s.Value
			}
		}
		return ""
	}

	modified := find("vcs.modified")
	if modified == "true" {
		isDevel = true
	}
}

// IsDevel returns true if the windows spawned using app will have the .devel
// class. This returns true if Go reports that vcs.modified is true.
func IsDevel() bool { return isDevel }

/*
	_diamondburned_ — Today at 16:52
		wow ctx abuse is so fun
		I can't wait until I lose scope of which context has which

	Corporate Shill (SAY NO TO GORM) — Today at 16:58
		This is why you dont do that
		Aaaaaaaaa
		The java compiler does it as well
		Painful
*/

var appHooks []func(*Application)

// Hook adds a new hook callback to be called during construction of an
// Application. This function allows injecting initialization function calls
// such as Adwaita's without having to explicitly write it in main().
func Hook(f func(*Application)) {
	appHooks = append(appHooks, f)
}

// Application describes the state of a Matrix application.
type Application struct {
	*gtk.Application
	ctx  context.Context // non-nil if Run
	name string

	configPath lazyString
	cacheDir   lazyString
}

type ctxKey uint

const (
	applicationKey ctxKey = iota
	windowKey      ctxKey = iota
)

// WithApplication injects the given application instance into a context. The
// returned context will also be cancelled if the application shuts down.
func WithApplication(ctx context.Context, app *Application) context.Context {
	ctx = context.WithValue(ctx, applicationKey, app)

	ctx, cancel := context.WithCancel(ctx)
	app.ConnectShutdown(cancel)

	return ctx
}

// FromContext pulls the application from the given context. If the given
// context isn't derived from Application, then nil is returned.
func FromContext(ctx context.Context) *Application {
	app, _ := ctx.Value(applicationKey).(*Application)
	return app
}

// IsActive returns true if any of the windows belonging to gotktrix is active.
func IsActive(ctx context.Context) bool {
	app := FromContext(ctx)
	for _, win := range app.Windows() {
		if win.IsActive() {
			return true
		}
	}
	return false
}

// New creates a new Application.
func New(ctx context.Context, appID, appName string) *Application {
	return NewWithFlags(ctx, appID, appName, gio.ApplicationFlagsNone)
}

// NewWithFlags creates a new Application with the given application flags.
func NewWithFlags(ctx context.Context, appID, appName string, flags gio.ApplicationFlags) *Application {
	if ctx == nil {
		panic("app: given ctx is nil")
	}

	app := &Application{
		Application: gtk.NewApplication(appID, flags),
		name:        appName,
	}

	ctx, cancel := context.WithCancel(ctx)
	app.ctx = ctx
	app.Application.ConnectShutdown(cancel)

	app.Application.ConnectStartup(func() {
		// TODO: make this display-bound. gtkutil has code for that.
		cssutil.ApplyGlobalCSS()
		cssutil.ApplyUserCSS(app.ConfigPath("user.css"))
	})

	app.cacheDir = newLazyString(func() string {
		d, err := os.UserCacheDir()
		if err != nil {
			d = os.TempDir()
			log.Println("cannot get user cache directory; falling back to", d)
		}

		cacheDir := filepath.Join(d, app.BaseID())

		if err := os.MkdirAll(cacheDir, 0755); err != nil {
			log.Println("error making config dir:", err)
		}

		return cacheDir
	})

	app.configPath = newLazyString(func() string {
		d, err := os.UserConfigDir()
		if err != nil {
			log.Fatalln("failed to get user config dir:", err)
		}

		configPath := filepath.Join(d, app.BaseID())

		// Enforce the right permissions.
		if err := os.MkdirAll(configPath, 0755); err != nil {
			log.Println("error making config dir:", err)
		}

		return configPath
	})

	for _, hook := range appHooks {
		hook(app)
	}

	return app
}

func (app *Application) assert() {
	if app == nil {
		panic("app == nil")
	}
}

// Error calls Error on the application inside the context. It panics if the
// context does not have the application.
func Error(ctx context.Context, errs ...error) {
	for _, err := range errs {
		log.Println("error:", err)
	}

	if app := FromContext(ctx); app != nil {
		app.Error(errs...)
	}
}

// Fatal is similar to Error, but calls Fatal instead.
func Fatal(ctx context.Context, errs ...error) {
	for _, err := range errs {
		log.Println("fatal:", err)
	}

	if win := WindowFromContext(ctx); win != nil {
		win.Fatal(errs...)
		return
	}

	if app := FromContext(ctx); app != nil {
		app.Fatal(errs...)
		return
	}

	panic("fatal error(s) occured")
}

// Error shows an error popup.
func (app *Application) Error(err ...error) {
	errpopup.Show(app.ActiveWindow(), filterAndLogErrors("error:", err), func() {})
}

// Fatal shows a fatal error popup and closes the application afterwards.
func (app *Application) Fatal(err ...error) {
	for _, win := range app.Windows() {
		win.SetSensitive(false)
		errpopup.Show(win, filterAndLogErrors("fatal:", err), app.Quit)
	}
}

func filterAndLogErrors(prefix string, errors []error) []error {
	nonNils := errors[:0]

	for _, err := range errors {
		if err == nil {
			continue
		}
		nonNils = append(nonNils, err)
		log.Println(prefix, err)
	}

	return nonNils
}

// Context returns the Application's context. This context contains the
// Application instance and will be cancelled on SIGINT once Run has been
// called. This method will panic if it's called before Run is called. Use it
// during initialization within signals.
func (app *Application) Context() context.Context {
	return app.ctx
}

// Quit quits the application. The function is thread-safe.
func (app *Application) Quit() {
	glib.IdleAddPriority(coreglib.PriorityHigh, app.Application.Quit)
}

// Run runs the application for as long as the context is alive. If a SIGINT is
// sent, then the application is stopped.
func (app *Application) Run(args []string) int {
	ctx, cancel := signal.NotifyContext(app.ctx, os.Interrupt)
	defer cancel()

	app.ctx = WithApplication(ctx, app)

	go func() {
		<-ctx.Done()
		app.Quit()
	}()

	return app.Application.Run(args)
}

// RunMain is a convenient function around Run that uses os.Args and os.Exit.
func (app *Application) RunMain() {
	os.Exit(app.Run(os.Args))
}

// NewWindow creates a new Window bounded to the Application instance.
func (app *Application) NewWindow() *Window {
	window := gtk.NewApplicationWindow(app.Application)
	window.SetDefaultSize(600, 400)

	// Initialize the scale factor state.
	gtkutil.ScaleFactor()

	if isDevel {
		window.AddCSSClass("devel")
	}

	w := Window{
		Window: window.Window,
		app:    app,
	}
	w.SetLoading()

	return &w
}

// AddActionShortcuts binds accelerator strings to action strings.
func (app *Application) AddActionShortcuts(m map[string]string) {
	actionAccels := make(map[string][]string, len(m))
	for accel, action := range m {
		if !strings.HasPrefix(action, "app.") {
			action = "app." + action
		}
		actionAccels[action] = append(actionAccels[action], accel)
	}

	for action, accels := range actionAccels {
		app.Application.SetAccelsForAction(action, accels)
	}

	gtkutil.Async(app.ctx, func() func() {
		var changed bool

		cfg := acquireState(app.ConfigPath("app-shortcuts"))
		cfg.Each(func(k string, unmarshal func(interface{}) bool) bool {
			if !strings.HasPrefix(k, "app.") {
				log.Printf("app-shortcuts: action %q not prefixed with app, skipping", k)
				return false
			}

			var action string
			if !unmarshal(action) {
				return false
			}

			actionAccels[k] = append(actionAccels[k], action)
			changed = true

			return false
		})

		if !changed {
			return nil
		}

		return func() {
			for action, accels := range actionAccels {
				app.Application.SetAccelsForAction(action, accels)
			}
		}
	})

}

// AddActions adds the given map of actions into the Application.
func (app *Application) AddActions(m map[string]func()) {
	for name, fn := range m {
		name = strings.TrimPrefix(name, "app.")

		c := gtkutil.NewCallbackAction(name)
		c.OnActivate(fn)
		app.AddAction(c)
	}
}

// AddJSONActions adds the given map of action functions with parameters into
// the Application. The value of the map is a function of type `func(T)`, where
// T is any type that is JSON-encoded using gtkutil.NewJSONVariant.
func (app *Application) AddJSONActions(m map[string]interface{}) {
	for name, fn := range m {
		name = strings.TrimPrefix(name, "app.")

		if callback, ok := fn.(func()); ok {
			action := gtkutil.NewCallbackAction(name)
			action.OnActivate(callback)

			app.AddAction(action)
		} else {
			callback := gtkutil.NewJSONActionCallback(fn)

			action := gio.NewSimpleAction(name, callback.ArgType)
			action.ConnectActivate(callback.Func)
			app.AddAction(action)
		}
	}
}

// AddActionCallbacks is the ActionCallback variant of AddActions.
func (app *Application) AddActionCallbacks(m map[string]gtkutil.ActionCallback) {
	for name, callback := range m {
		name = strings.TrimPrefix(name, "app.")

		action := gio.NewSimpleAction(name, callback.ArgType)
		action.ConnectActivate(callback.Func)
		app.AddAction(action)
	}
}

// ID returns the application ID.
func (app *Application) ID() string {
	return app.Application.ApplicationID()
}

// IDDot creates a new application ID by joining all parts into the tail of the
// application ID. If no arguments are given, then the app ID is returned.
func (app *Application) IDDot(parts ...string) string {
	if len(parts) == 0 {
		return app.ID()
	}
	return app.ID() + "." + strings.Join(parts, ".")
}

// BaseID returns the last part of the application ID.
func (app *Application) BaseID() string {
	parts := strings.Split(app.ID(), ".")
	return parts[len(parts)-1]
}

// Name returns the application name.
func (app *Application) Name() string {
	return app.name
}

// SuffixedTitle suffixes the title with the application name and returns the
// string.
func (app *Application) SuffixedTitle(title string) string {
	if title == "" {
		return app.name
	}
	return title + " — " + app.name
}

// ConfigPath returns the path to the configuration directory with the given
// tails appended. If the path fails, then the function panics.
func (app *Application) ConfigPath(tails ...string) string {
	app.assert()
	return joinTails(app.configPath.v(), tails)
}

// CacheDir returns the path to the cache directory of the application.
func (app *Application) CachePath(tails ...string) string {
	app.assert()
	return joinTails(app.cacheDir.v(), tails)
}

func joinTails(dir string, tails []string) string {
	if len(tails) == 1 {
		dir = filepath.Join(dir, tails[0])
	} else if len(tails) > 0 {
		paths := append([]string{dir}, tails...)
		dir = filepath.Join(paths...)
	}

	return dir
}

type lazyString struct {
	str  string
	fun  func() string
	once sync.Once
}

func newLazyString(f func() string) lazyString {
	return lazyString{fun: f}
}

func (l lazyString) v() string {
	l.once.Do(func() {
		l.str = l.fun()
	})
	return l.str
}
