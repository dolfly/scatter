// SPDX-License-Identifier: Unlicense OR MIT

package main

import (
	"fmt"
	"image"
	"image/color"
	"log"
	"math"
	"runtime"
	"strings"
	"time"

	"golang.org/x/image/draw"

	"gioui.org/ui"
	"gioui.org/ui/app"
	gdraw "gioui.org/ui/draw"
	"gioui.org/ui/f32"
	"gioui.org/ui/gesture"
	"gioui.org/ui/input"
	"gioui.org/ui/key"
	"gioui.org/ui/layout"
	"gioui.org/ui/measure"
	"gioui.org/ui/pointer"
	"gioui.org/ui/system"
	"gioui.org/ui/text"
	"gioui.org/ui/widget"
	"golang.org/x/exp/shiny/iconvg"

	"golang.org/x/image/font/gofont/gobold"
	"golang.org/x/image/font/gofont/goitalic"
	"golang.org/x/image/font/gofont/gomono"
	"golang.org/x/image/font/gofont/goregular"
	"golang.org/x/image/font/sfnt"

	"golang.org/x/exp/shiny/materialdesign/icons"
)

type Env struct {
	insets app.Insets
	faces  measure.Faces
	client *Client
	redraw func()
}

type App struct {
	w   *app.Window
	env Env

	stack pageStack

	// Profiling.
	profiling   bool
	profile     system.ProfileEvent
	lastMallocs uint64
}

type pageStack struct {
	pages    []Page
	stopChan chan<- struct{}
}

type Page interface {
	Start(stop <-chan struct{})
	Event(c ui.Config, q input.Queue) interface{}
	Layout(c ui.Config, q input.Queue, ops *ui.Ops, cs layout.Constraints) layout.Dimens
}

type signInPage struct {
	env     *Env
	account *Account
	list    *layout.List
	fields  []*formField
	submit  *Button
}

type Button struct {
	Label string
	click gesture.Click
}

type Topbar struct {
	Back bool

	backClick gesture.Click
	stack     layout.Stack
	insets    layout.Inset
	insets2   layout.Inset
	flex      layout.Flex
	backChild layout.FlexChild
	bg        layout.StackChild
}

type formField struct {
	env    *Env
	Header string
	Hint   string
	Value  *string
	edit   *text.Editor
}

type threadsPage struct {
	env     *Env
	account *Account

	fab *IconButton

	updates       <-chan struct{}
	threadUpdates chan []*Thread

	list *layout.List

	threads []*Thread
	clicks  []gesture.Click
}

type threadPage struct {
	env       *Env
	checkmark *icon
	thread    *Thread
	list      *layout.List
	messages  []*Message
	result    chan []*Message
	msgEdit   *text.Editor
	send      *IconButton
	invite    *Button
	accept    *Button
	topbar    *Topbar
	updates   <-chan struct{}
}

type contactsPage struct {
	env        *Env
	list       *layout.List
	searchEdit *text.Editor
	contacts   []*Contact
	clicks     []gesture.Click
	query      chan []*Contact
	topbar     *Topbar
}

type Contact struct {
	Address string
}

type icon struct {
	src  []byte
	size ui.Value

	// Cached values.
	img     image.Image
	imgSize int
}

type IconButton struct {
	Icon  *icon
	Inset layout.Inset
	click gesture.Click
}

type BackEvent struct{}

type SignInEvent struct {
	Account *Account
}

type NewThreadEvent struct {
	Address string
}

type ShowContactsEvent struct{}

type ShowThreadEvent struct {
	Thread string
}

var fonts struct {
	regular *sfnt.Font
	bold    *sfnt.Font
	italic  *sfnt.Font
	mono    *sfnt.Font
}

var theme struct {
	text     ui.MacroOp
	tertText ui.MacroOp
	brand    ui.MacroOp
	white    ui.MacroOp
}

func uiMain() {
	app.Main()
}

func init() {
	fonts.regular = mustLoadFont(goregular.TTF)
	fonts.bold = mustLoadFont(gobold.TTF)
	fonts.italic = mustLoadFont(goitalic.TTF)
	fonts.mono = mustLoadFont(gomono.TTF)
	var ops ui.Ops
	theme.text = colorMaterial(&ops, rgb(0x000000))
	theme.tertText = colorMaterial(&ops, rgb(0xbbbbbb))
	theme.brand = colorMaterial(&ops, rgb(0x3c98c6))
	theme.white = colorMaterial(&ops, rgb(0xffffff))
	go func() {
		w := app.NewWindow(&app.WindowOptions{
			Width:  ui.Dp(400),
			Height: ui.Dp(800),
			Title:  "Scatter",
		})
		if err := newApp(w).run(); err != nil {
			log.Fatal(err)
		}
	}()
}

func colorMaterial(ops *ui.Ops, color color.RGBA) ui.MacroOp {
	var mat ui.MacroOp
	mat.Record(ops)
	gdraw.ColorOp{Color: color}.Add(ops)
	mat.Stop()
	return mat
}

func (a *App) run() error {
	var updates <-chan struct{}
	ops := new(ui.Ops)
	var cfg ui.Config
	for {
		select {
		case <-updates:
			if err := a.env.client.Err(); err != nil {
				log.Printf("client err: %v", err)
				a.stack.Clear(newSignInPage(&a.env))
			}
			a.w.Invalidate()
		case e := <-a.w.Events():
			switch e := e.(type) {
			case key.ChordEvent:
				switch e.Name {
				case key.NameEscape:
					if a.stack.Len() > 1 {
						a.stack.Pop()
						a.w.Invalidate()
					}
				case 'P':
					if e.Modifiers&key.ModCommand != 0 {
						a.profiling = !a.profiling
						a.w.Invalidate()
					}
				}
			case app.DestroyEvent:
				return e.Err
			case app.StageEvent:
				if e.Stage >= app.StageRunning {
					if a.env.client == nil {
						a.env.client = getClient()
						updates = a.env.client.register(a)
						defer a.env.client.unregister(a)
					}
					if a.stack.Len() == 0 {
						a.stack.Push(newThreadsPage(&a.env))
					}
				}
			case *app.CommandEvent:
				switch e.Type {
				case app.CommandBack:
					if a.stack.Len() > 1 {
						a.stack.Pop()
						e.Cancel = true
						a.w.Invalidate()
					}
				}
			case app.DrawEvent:
				ops.Reset()
				cfg = &e.Config
				q := a.w.Queue()
				a.env.faces.Reset(cfg)
				a.env.insets = e.Insets
				cs := layout.RigidConstraints(e.Size)
				a.Layout(cfg, q, ops, cs)
				if a.profiling {
					a.layoutTimings(cfg, q, ops, cs)
				}
				a.w.Draw(ops)
			}
		}
	}
}

func newApp(w *app.Window) *App {
	a := &App{
		w: w,
	}
	a.env.redraw = a.w.Invalidate
	return a
}

func (s *pageStack) Len() int {
	return len(s.pages)
}

func (s *pageStack) Current() Page {
	return s.pages[len(s.pages)-1]
}

func (s *pageStack) Pop() {
	s.stop()
	i := len(s.pages) - 1
	s.pages[i] = nil
	s.pages = s.pages[:i]
	if len(s.pages) > 0 {
		s.start()
	}
}

func (s *pageStack) start() {
	stop := make(chan struct{})
	s.stopChan = stop
	s.Current().Start(stop)
}

func (s *pageStack) Push(p Page) {
	if s.stopChan != nil {
		s.stop()
	}
	s.pages = append(s.pages, p)
	s.start()
}

func (s *pageStack) stop() {
	close(s.stopChan)
	s.stopChan = nil
}

func (s *pageStack) Clear(p Page) {
	for len(s.pages) > 0 {
		s.Pop()
	}
	s.Push(p)
}

func mustLoadFont(fontData []byte) *sfnt.Font {
	fnt, err := sfnt.Parse(fontData)
	if err != nil {
		panic("failed to load font")
	}
	return fnt
}

func rgb(c uint32) color.RGBA {
	return argb((0xff << 24) | c)
}

func argb(c uint32) color.RGBA {
	return color.RGBA{A: uint8(c >> 24), R: uint8(c >> 16), G: uint8(c >> 8), B: uint8(c)}
}

func (a *App) Layout(c ui.Config, q input.Queue, ops *ui.Ops, cs layout.Constraints) layout.Dimens {
	a.update(c, q)
	return a.stack.Current().Layout(c, q, ops, cs)
}

func (a *App) layoutTimings(c ui.Config, q input.Queue, ops *ui.Ops, cs layout.Constraints) layout.Dimens {
	for _, e := range q.Events(a) {
		if e, ok := e.(system.ProfileEvent); ok {
			a.profile = e
		}
	}

	system.ProfileOp{Key: a}.Add(ops)
	var mstats runtime.MemStats
	runtime.ReadMemStats(&mstats)
	mallocs := mstats.Mallocs - a.lastMallocs
	a.lastMallocs = mstats.Mallocs
	al := layout.Align{Alignment: layout.NE}
	cs = al.Begin(ops, cs)
	in := layout.Inset{Top: ui.Dp(16)}
	in2 := layout.Inset{Top: a.env.insets.Top}
	cs = in.Begin(c, ops, cs)
	cs = in2.Begin(c, ops, cs)
	txt := fmt.Sprintf("m: %d %s", mallocs, a.profile.Timings)
	dims := text.Label{Material: theme.text, Face: a.env.faces.For(fonts.mono, ui.Sp(10)), Text: txt}.Layout(ops, cs)
	dims = in2.End(dims)
	dims = in.End(dims)
	return al.End(dims)
}

func newContactsPage(env *Env) *contactsPage {
	p := &contactsPage{
		env: env,
		list: &layout.List{
			Axis: layout.Vertical,
		},
		searchEdit: &text.Editor{
			Face:         env.faces.For(fonts.regular, ui.Sp(20)),
			SingleLine:   true,
			Submit:       true,
			Hint:         "Email address",
			Material:     theme.white,
			HintMaterial: theme.tertText,
		},
		topbar: &Topbar{
			Back: true,
		},
	}
	p.searchEdit.Focus()
	return p
}

func (p *contactsPage) Start(stop <-chan struct{}) {}

func (p *contactsPage) Event(c ui.Config, q input.Queue) interface{} {
	for {
		e, ok := p.searchEdit.Next(c, q)
		if !ok {
			break
		}
		switch e.(type) {
		case text.ChangeEvent:
			p.queryContacts(p.searchEdit.Text())
		case text.SubmitEvent:
			if t := p.searchEdit.Text(); isEmailAddress(t) {
				return NewThreadEvent{Address: t}
			}
		}
	}
	select {
	case p.contacts = <-p.query:
		p.clicks = make([]gesture.Click, len(p.contacts))
	default:
	}
	for i := range p.clicks {
		for _, e := range p.clicks[i].Events(q) {
			if e.Type == gesture.TypeClick {
				return NewThreadEvent{p.contacts[i].Address}
			}
		}
	}
	return p.topbar.Event(c, q)
}

func isEmailAddress(e string) bool {
	idx := strings.Index(e, "@")
	return idx > 0 && idx < len(e)-1
}

func (p *contactsPage) queryContacts(q string) {
	p.query = make(chan []*Contact, 1)
	go func() {
		var contacts []*Contact
		if isEmailAddress(q) {
			contacts = append(contacts, &Contact{Address: q})
		}
		threads, err := p.env.client.QueryThreads(q)
		if err == nil {
			for _, t := range threads {
				contacts = append(contacts, &Contact{Address: t.ID})
			}
		} else {
			log.Printf("queryContacts: failed to query threads: %v", err)
		}
		p.query <- contacts
		p.env.redraw()
	}()
}

func (p *contactsPage) Layout(c ui.Config, q input.Queue, ops *ui.Ops, cs layout.Constraints) layout.Dimens {
	for e := p.Event(c, q); e != nil; e = p.Event(c, q) {
	}
	l := p.list
	if l.Dragging() {
		key.HideInputOp{}.Add(ops)
	}
	f := layout.Flex{Axis: layout.Vertical, MainAxisAlignment: layout.Start}
	f.Init(ops, cs)
	var dims layout.Dimens
	cs = f.Rigid()
	{
		cs = p.topbar.Begin(c, p.env.insets, ops, cs)
		dims = p.searchEdit.Layout(c, q, ops, cs)
		dims = p.topbar.End(dims)
	}
	c1 := f.End(dims)
	cs = f.Flexible(1)
	cs.Height.Min = cs.Height.Max
	sysInset := layout.Inset{
		Left:  p.env.insets.Left,
		Right: p.env.insets.Right,
	}
	cs = sysInset.Begin(c, ops, cs)
	for l.Init(c, q, ops, cs, len(p.contacts)); l.More(); l.Next() {
		l.Elem(p.contact(c, ops, l.Constraints(), l.Index()))
	}
	dims = l.Layout()
	dims = sysInset.End(dims)
	c2 := f.End(dims)
	return f.Layout(c1, c2)
}

func (p *contactsPage) contact(c ui.Config, ops *ui.Ops, cs layout.Constraints, index int) layout.Dimens {
	contact := p.contacts[index]
	click := &p.clicks[index]
	in := layout.UniformInset(ui.Dp(8))
	cs = in.Begin(c, ops, cs)
	f := (&layout.Flex{CrossAxisAlignment: layout.Center}).Init(ops, cs)
	var dims layout.Dimens
	{
		cs = f.Rigid()
		in := layout.Inset{Right: ui.Dp(8)}
		cc := clipCircle{}
		cs = cc.Begin(ops, in.Begin(c, ops, cs))
		sz := image.Point{X: c.Px(ui.Dp(48)), Y: c.Px(ui.Dp(48))}
		cs = layout.RigidConstraints(cs.Constrain(sz))
		dims = fill{theme.brand}.Layout(ops, cs)
		dims = in.End(cc.End(dims))
	}
	c1 := f.End(dims)
	{
		cs = f.Flexible(1)
		dims = text.Label{Material: theme.text, Face: p.env.faces.For(fonts.regular, ui.Sp(18)), Text: contact.Address}.Layout(ops, cs)
	}
	c2 := f.End(dims)

	dims = f.Layout(c1, c2)
	dims = in.End(dims)
	pointer.RectAreaOp{Size: dims.Size}.Add(ops)
	click.Add(ops)
	return dims
}

func (t *Topbar) Event(c ui.Config, queue input.Queue) interface{} {
	for _, e := range t.backClick.Events(queue) {
		if e.Type == gesture.TypeClick {
			return BackEvent{}
		}
	}
	return nil
}

func (t *Topbar) Begin(c ui.Config, insets app.Insets, ops *ui.Ops, cs layout.Constraints) layout.Constraints {
	t.stack = layout.Stack{Alignment: layout.W}
	topInset := insets.Top
	t.insets2 = layout.Inset{
		Top:   topInset,
		Left:  insets.Left,
		Right: insets.Right,
	}
	t.stack.Init(ops, cs)
	cs = t.stack.Rigid()
	if h := c.Px(topInset) + c.Px(ui.Dp(56)); h < cs.Height.Max {
		cs.Height.Max = h
	}
	dims := fill{theme.brand}.Layout(ops, cs)
	t.bg = t.stack.End(dims)
	cs = t.stack.Rigid()
	cs = t.insets2.Begin(c, ops, cs)
	t.flex = layout.Flex{CrossAxisAlignment: layout.Center}
	t.flex.Init(ops, cs)
	cs = t.flex.Rigid()
	dims = layout.Dimens{}
	t.insets = layout.UniformInset(ui.Dp(12))
	if t.Back {
		t.insets.Left = ui.Px(0)
		ico := (&icon{src: icons.NavigationArrowBack, size: ui.Dp(24)}).image(c, rgb(0xffffff))
		in := layout.UniformInset(ui.Dp(8))
		cs = in.Begin(c, ops, cs)
		dims = widget.Image{Src: ico, Rect: ico.Bounds(), Scale: 1}.Layout(c, ops, cs)
		dims = in.End(dims)
		pointer.RectAreaOp{Size: dims.Size}.Add(ops)
		t.backClick.Add(ops)
	}
	t.backChild = t.flex.End(dims)
	cs = t.flex.Flexible(1)
	return t.insets.Begin(c, ops, cs)
}

func (t *Topbar) End(dims layout.Dimens) layout.Dimens {
	dims = t.insets.End(dims)
	content := t.flex.End(dims)
	dims = t.flex.Layout(t.backChild, content)
	dims = t.insets2.End(dims)
	stackContent := t.stack.End(dims)
	return t.stack.Layout(t.bg, stackContent)
}

func newSignInPage(env *Env) *signInPage {
	acc := env.client.Account()
	p := &signInPage{
		env:     env,
		account: acc,
		list: &layout.List{
			Axis: layout.Vertical,
		},
		fields: []*formField{
			&formField{Header: "Email address", Hint: "you@example.org", Value: &acc.User},
			&formField{Header: "Password", Hint: "correct horse battery staple", Value: &acc.Password},
			&formField{Header: "IMAP host", Hint: "imap.gmail.com:993", Value: &acc.IMAPHost},
			&formField{Header: "SMTP host", Hint: "smtp.gmail.com:587", Value: &acc.SMTPHost},
		},
		submit: &Button{
			Label: "Sign in",
		},
	}
	for _, f := range p.fields {
		f.env = p.env
		f.edit = &text.Editor{
			Face:         env.faces.For(fonts.regular, ui.Sp(16)),
			SingleLine:   true,
			Hint:         f.Hint,
			Material:     theme.text,
			HintMaterial: theme.tertText,
		}
		f.edit.SetText(*f.Value)
	}
	return p
}

func (p *signInPage) Start(stop <-chan struct{}) {
}

func (p *signInPage) Event(c ui.Config, q input.Queue) interface{} {
	for _, e := range p.submit.Events(q) {
		if e.Type == gesture.TypeClick {
			for _, f := range p.fields {
				*f.Value = f.edit.Text()
			}
			return SignInEvent{p.account}
		}
	}
	return nil
}

func (p *signInPage) Layout(c ui.Config, q input.Queue, ops *ui.Ops, cs layout.Constraints) layout.Dimens {
	var dims layout.Dimens
	f := layout.Flex{Axis: layout.Vertical, MainAxisAlignment: layout.Start}
	f.Init(ops, cs)

	cs = f.Rigid()
	{
		var t Topbar
		cs = t.Begin(c, p.env.insets, ops, cs)
		dims = text.Label{Material: colorMaterial(ops, rgb(0xffffff)), Face: p.env.faces.For(fonts.regular, ui.Sp(16)), Text: "Sign in"}.Layout(ops, cs)
		dims = t.End(dims)
	}
	c1 := f.End(dims)

	cs = f.Flexible(1)
	sysInset := layout.Inset{
		Left:   p.env.insets.Left,
		Right:  p.env.insets.Right,
		Bottom: p.env.insets.Bottom,
	}
	cs = sysInset.Begin(c, ops, cs)
	dims = p.layoutSigninForm(c, q, ops, cs)
	dims = sysInset.End(dims)
	c2 := f.End(dims)
	return f.Layout(c1, c2)
}

func (p *signInPage) layoutSigninForm(c ui.Config, q input.Queue, ops *ui.Ops, cs layout.Constraints) layout.Dimens {
	l := p.list
	for l.Init(c, q, ops, cs, len(p.fields)+1); l.More(); l.Next() {
		in := layout.Inset{Left: ui.Dp(32), Right: ui.Dp(32)}
		var dims layout.Dimens
		switch {
		case l.Index() < len(p.fields):
			in.Bottom = ui.Dp(12)
			if l.Index() == 0 {
				in.Top = ui.Dp(32)
			}
			cs = in.Begin(c, ops, l.Constraints())
			dims = p.fields[l.Index()].Layout(c, q, ops, cs)
			dims = in.End(dims)
		default:
			in.Bottom = ui.Dp(32)
			align := layout.Align{Alignment: layout.E}
			cs = in.Begin(c, ops, align.Begin(ops, cs))
			dims = p.submit.Layout(c, p.env, ops, cs)
			dims = align.End(in.End(dims))
		}
		l.Elem(dims)
	}
	return l.Layout()
}

func (f *formField) Layout(c ui.Config, q input.Queue, ops *ui.Ops, cs layout.Constraints) layout.Dimens {
	theme.text.Add(ops)
	fl := (&layout.Flex{Axis: layout.Vertical}).Init(ops, cs)

	header := text.Label{Material: theme.text, Text: f.Header, Face: f.env.faces.For(fonts.bold, ui.Sp(12))}
	cs = fl.Rigid()
	cs.Width.Min = cs.Width.Max
	dims := header.Layout(ops, cs)
	dims.Size.Y += c.Px(ui.Dp(4))
	c1 := fl.End(dims)
	c2 := fl.End(f.edit.Layout(c, q, ops, fl.Rigid()))
	dims = fl.Layout(c1, c2)
	return dims
}

func (b *Button) Events(queue input.Queue) []gesture.ClickEvent {
	return b.click.Events(queue)
}

func (b *Button) Layout(c ui.Config, env *Env, ops *ui.Ops, cs layout.Constraints) layout.Dimens {
	bg := Background{
		Material: theme.brand,
		Radius:   ui.Dp(4),
		Inset:    layout.UniformInset(ui.Dp(8)),
	}
	cs = bg.Begin(c, ops, cs)
	lbl := text.Label{Material: theme.white, Face: env.faces.For(fonts.regular, ui.Sp(16)), Text: b.Label, Alignment: text.Center}
	dims := lbl.Layout(ops, cs)
	dims = bg.End(dims)
	pointer.RectAreaOp{Size: dims.Size}.Add(ops)
	b.click.Add(ops)
	return dims
}

type Background struct {
	Material ui.MacroOp
	Radius   ui.Value
	Inset    layout.Inset

	macro ui.MacroOp
	c     ui.Config
	ops   *ui.Ops
}

func (b *Background) Begin(c ui.Config, ops *ui.Ops, cs layout.Constraints) layout.Constraints {
	b.c = c
	b.ops = ops
	b.macro.Record(ops)
	return b.Inset.Begin(c, ops, cs)
}

func (b *Background) End(dims layout.Dimens) layout.Dimens {
	dims = b.Inset.End(dims)
	b.macro.Stop()
	var stack ui.StackOp
	stack.Push(b.ops)
	w, h := float32(dims.Size.X), float32(dims.Size.Y)
	if r := float32(b.c.Px(b.Radius)); r > 0 {
		if r > w/2 {
			r = w / 2
		}
		if r > h/2 {
			r = h / 2
		}
		rrect(b.ops, w, h, r, r, r, r)
	}
	b.Material.Add(b.ops)
	gdraw.DrawOp{Rect: f32.Rectangle{Max: f32.Point{X: w, Y: h}}}.Add(b.ops)
	b.macro.Add(b.ops)
	stack.Pop()
	return dims
}

func newThreadsPage(env *Env) *threadsPage {
	return &threadsPage{
		env: env,
		list: &layout.List{
			Axis: layout.Vertical,
		},
		fab: &IconButton{
			Icon:  &icon{src: icons.ContentCreate, size: ui.Dp(24)},
			Inset: layout.UniformInset(ui.Dp(16)),
		},
	}
}

func (p *threadsPage) Start(stop <-chan struct{}) {
	p.account = p.env.client.Account()
	p.fetchThreads()
	p.updates = p.env.client.register(p)
	go func() {
		<-stop
		p.env.client.unregister(p)
	}()
}

func (p *threadsPage) Event(c ui.Config, q input.Queue) interface{} {
	select {
	case <-p.updates:
		p.fetchThreads()
	case threads := <-p.threadUpdates:
		p.threads = threads
		p.clicks = make([]gesture.Click, len(threads))
		p.env.redraw()
	default:
	}
	for _, e := range p.fab.Events(q) {
		if e.Type == gesture.TypeClick {
			return ShowContactsEvent{}
		}
	}
	for i := range p.clicks {
		click := &p.clicks[i]
		for _, e := range click.Events(q) {
			if e.Type == gesture.TypeClick {
				t := p.threads[i]
				return ShowThreadEvent{Thread: t.ID}
			}
		}
	}
	return nil
}

func (p *threadsPage) fetchThreads() {
	p.threadUpdates = make(chan []*Thread, 1)
	go func() {
		threads, err := p.env.client.Threads()
		if err != nil {
			log.Printf("scatter: failed to load threads: %v", err)
			return
		}
		p.threadUpdates <- threads
		p.env.redraw()
	}()
}

func (p *threadsPage) Layout(c ui.Config, q input.Queue, ops *ui.Ops, cs layout.Constraints) layout.Dimens {
	st := layout.Stack{Alignment: layout.Center}
	st.Init(ops, cs)

	sysInset := layout.Inset{
		Left:  p.env.insets.Left,
		Right: p.env.insets.Right,
	}
	var dims layout.Dimens
	cs = st.Rigid()
	{
		f := layout.Flex{Axis: layout.Vertical, MainAxisAlignment: layout.Start}
		f.Init(ops, cs)

		cs = f.Rigid()
		{
			var t Topbar
			cs = t.Begin(c, p.env.insets, ops, cs)
			dims = text.Label{Material: theme.white, Face: p.env.faces.For(fonts.regular, ui.Sp(20)), Text: p.account.User}.Layout(ops, cs)
			dims = t.End(dims)
		}
		c3 := f.End(dims)

		cs = f.Flexible(1)
		cs = sysInset.Begin(c, ops, cs)
		dims = p.layoutThreads(c, q, ops, cs)
		dims = sysInset.End(dims)
		c4 := f.End(dims)
		dims = f.Layout(c3, c4)
	}
	c1 := st.End(dims)
	cs = st.Rigid()
	sysInset.Bottom = p.env.insets.Bottom
	cs = sysInset.Begin(c, ops, cs)
	al := layout.Align{Alignment: layout.SE}
	in := layout.UniformInset(ui.Dp(16))
	cs = in.Begin(c, ops, al.Begin(ops, cs))
	dims = p.fab.Layout(c, p.env, ops, cs)
	dims = al.End(in.End(dims))
	dims = sysInset.End(dims)
	c2 := st.End(dims)
	dims = st.Layout(c1, c2)
	return dims
}

func (p *threadsPage) layoutThreads(c ui.Config, q input.Queue, ops *ui.Ops, cs layout.Constraints) layout.Dimens {
	l := p.list
	if l.Dragging() {
		key.HideInputOp{}.Add(ops)
	}
	for l.Init(c, q, ops, cs, len(p.threads)); l.More(); l.Next() {
		in := layout.Inset{}
		switch l.Index() {
		case 0:
			in.Top = ui.Dp(4)
		case len(p.threads) - 1:
			in.Bottom = ui.Dp(4)
		}
		cs := in.Begin(c, ops, l.Constraints())
		dims := p.thread(c, ops, cs, l.Index())
		dims = in.End(dims)
		l.Elem(dims)
	}
	dims := l.Layout()
	return dims
}

func (p *threadsPage) thread(c ui.Config, ops *ui.Ops, cs layout.Constraints, index int) layout.Dimens {
	t := p.threads[index]
	bgtexmat := theme.tertText
	font := fonts.regular
	if t.Unread > 0 {
		bgtexmat = theme.text
		font = fonts.bold
	}
	click := &p.clicks[index]
	elem := layout.Flex{Axis: layout.Vertical, MainAxisAlignment: layout.Start, CrossAxisAlignment: layout.Start}
	elem.Init(ops, cs)
	cs = elem.Rigid()
	var dims layout.Dimens
	{
		in := layout.UniformInset(ui.Dp(8))
		cs = in.Begin(c, ops, cs)
		f := centerRowOpts()
		f.Init(ops, cs)
		cs = f.Rigid()
		{
			in := layout.Inset{Right: ui.Dp(12), Left: ui.Dp(4)}
			cc := clipCircle{}
			cs = cc.Begin(ops, in.Begin(c, ops, cs))
			sz := image.Point{X: c.Px(ui.Dp(48)), Y: c.Px(ui.Dp(48))}
			cs = layout.RigidConstraints(cs.Constrain(sz))
			dims = fill{theme.brand}.Layout(ops, cs)
			dims = in.End(cc.End(dims))
		}
		c1 := f.End(dims)
		cs = f.Rigid()
		{
			f := column()
			f.Init(ops, cs)
			cs = f.Rigid()
			{
				f := baseline()
				f.Init(ops, cs)
				cs = f.Rigid()
				dims = text.Label{Material: theme.text, Face: p.env.faces.For(font, ui.Sp(18)), Text: t.ID}.Layout(ops, cs)
				c1 := f.End(dims)
				cs = f.Flexible(1)
				cs.Width.Min = cs.Width.Max
				al := layout.Align{Alignment: layout.E}
				in := layout.Inset{Left: ui.Dp(2)}
				cs = in.Begin(c, ops, al.Begin(ops, cs))
				dims = text.Label{Material: bgtexmat, Face: p.env.faces.For(font, ui.Sp(12)), Text: formatTime(t.Updated)}.Layout(ops, cs)
				dims = al.End(in.End(dims))
				c2 := f.End(dims)
				dims = f.Layout(c1, c2)
			}
			c1 := f.End(dims)
			cs = f.Rigid()
			in := layout.Inset{Top: ui.Dp(6)}
			cs = in.Begin(c, ops, cs)
			dims = text.Label{Material: bgtexmat, Face: p.env.faces.For(font, ui.Sp(14)), MaxLines: 1, Text: t.Snippet}.Layout(ops, cs)
			dims = in.End(dims)
			c2 := f.End(dims)
			dims = f.Layout(c1, c2)
		}
		c2 := f.End(dims)
		dims = f.Layout(c1, c2)
		dims = in.End(dims)
		pointer.RectAreaOp{Size: dims.Size}.Add(ops)
		click.Add(ops)
	}
	c1 := elem.End(dims)
	return elem.Layout(c1)
}

func newThreadPage(env *Env, threadID string) *threadPage {
	thread := env.client.Thread(threadID)
	return &threadPage{
		env:       env,
		thread:    thread,
		checkmark: &icon{src: icons.ActionDone, size: ui.Dp(12)},
		list: &layout.List{
			Axis:   layout.Vertical,
			Invert: true,
		},
		result: make(chan []*Message, 1),
		msgEdit: &text.Editor{
			Face:         env.faces.For(fonts.regular, ui.Sp(14)),
			Submit:       true,
			Hint:         "Send a message",
			Material:     theme.text,
			HintMaterial: theme.tertText,
		},
		send: &IconButton{
			Icon:  &icon{src: icons.ContentSend, size: ui.Dp(24)},
			Inset: layout.UniformInset(ui.Dp(6)),
		},
		invite: &Button{
			Label: "Send invitation",
		},
		accept: &Button{
			Label: "Accept invitation",
		},
		topbar: &Topbar{
			Back: true,
		},
	}
}

func (p *threadPage) Start(stop <-chan struct{}) {
	p.fetchMessages()
	p.updates = p.env.client.register(p)
	p.env.client.MarkRead(p.thread.ID)
	go func() {
		<-stop
		p.env.client.unregister(p)
	}()
}

func (p *threadPage) Event(c ui.Config, q input.Queue) interface{} {
	select {
	case <-p.updates:
		p.fetchMessages()
	default:
	}
	for e, ok := p.msgEdit.Next(c, q); ok; e, ok = p.msgEdit.Next(c, q) {
		if _, ok := e.(text.SubmitEvent); ok {
			p.sendMessage()
		}
	}
	for _, e := range p.send.Events(q) {
		if e.Type == gesture.TypeClick {
			p.sendMessage()
		}
	}
	for _, e := range p.invite.Events(q) {
		if e.Type == gesture.TypeClick {
			if err := p.env.client.Send(p.thread.ID, "Invitation sent"); err != nil {
				log.Printf("failed to send invitation: %v", err)
			}
			break
		}
	}
	for _, e := range p.accept.Events(q) {
		if e.Type == gesture.TypeClick {
			if err := p.env.client.Send(p.thread.ID, "Invitation accepted"); err != nil {
				log.Printf("failed to send invitation accept: %v", err)
			}
			break
		}
	}
	return p.topbar.Event(c, q)
}

func (p *threadPage) sendMessage() {
	if t := p.msgEdit.Text(); t != "" {
		if err := p.env.client.Send(p.thread.ID, t); err != nil {
			log.Printf("failed to send message: %v", err)
		}
		p.msgEdit.SetText("")
	}
}

func (p *threadPage) Layout(c ui.Config, q input.Queue, ops *ui.Ops, cs layout.Constraints) layout.Dimens {
	l := p.list
	if l.Dragging() {
		key.HideInputOp{}.Add(ops)
	}
	select {
	case p.messages = <-p.result:
	default:
	}
	var dims layout.Dimens
	f := layout.Flex{Axis: layout.Vertical, MainAxisAlignment: layout.Start}
	f.Init(ops, cs)
	{
		cs = f.Rigid()
		cs = p.topbar.Begin(c, p.env.insets, ops, cs)
		dims = text.Label{Material: theme.white, Face: p.env.faces.For(fonts.regular, ui.Sp(20)), Text: p.thread.ID}.Layout(ops, cs)
		dims = p.topbar.End(dims)
	}
	c1 := f.End(dims)

	sysInset := layout.Inset{
		Left:  p.env.insets.Left,
		Right: p.env.insets.Right,
	}
	{
		cs = f.Rigid()
		sysInset := sysInset
		sysInset.Bottom = p.env.insets.Bottom
		cs = sysInset.Begin(c, ops, cs)
		in := layout.Inset{Top: ui.Dp(8), Bottom: ui.Dp(8), Left: ui.Dp(12), Right: ui.Dp(12)}
		cs = in.Begin(c, ops, cs)
		switch {
		case p.thread.PendingInvitation:
			dims = p.accept.Layout(c, p.env, ops, cs)
		case p.env.client.ContainsSession(p.thread.ID):
			dims = p.layoutMessageBox(c, q, ops, cs)
		default:
			dims = p.invite.Layout(c, p.env, ops, cs)
		}
		dims = in.End(dims)
		dims = sysInset.End(dims)
	}
	c3 := f.End(dims)

	{
		cs = f.Flexible(1)
		cs.Height.Min = cs.Height.Max
		cs = sysInset.Begin(c, ops, cs)
		for l.Init(c, q, ops, cs, len(p.messages)); l.More(); l.Next() {
			l.Elem(p.message(c, ops, l.Constraints(), l.Index()))
		}
		dims = l.Layout()
		dims = sysInset.End(dims)
	}
	c2 := f.End(dims)
	return f.Layout(c1, c2, c3)
}

func (p *threadPage) layoutMessageBox(c ui.Config, q input.Queue, ops *ui.Ops, cs layout.Constraints) layout.Dimens {
	if mh := c.Px(ui.Dp(100)); cs.Height.Max > mh {
		cs.Height.Max = mh
	}
	f := (&layout.Flex{CrossAxisAlignment: layout.End}).Init(ops, cs)

	var dims layout.Dimens
	var sendHeight int
	{
		cs = f.Rigid()
		in := layout.Inset{Left: ui.Dp(8)}
		cs = in.Begin(c, ops, cs)
		dims = p.send.Layout(c, p.env, ops, cs)
		sendHeight = dims.Size.Y
		dims = in.End(dims)
	}
	c2 := f.End(dims)

	{
		cs = f.Flexible(1)
		cs.Width.Min = cs.Width.Max
		if cs.Height.Min < sendHeight {
			cs.Height.Min = sendHeight
		}
		bg := Background{
			Material: colorMaterial(ops, rgb(0xeeeeee)),
			Inset:    layout.UniformInset(ui.Dp(8)),
			Radius:   ui.Dp(10),
		}
		cs = bg.Begin(c, ops, cs)
		align := layout.Align{Alignment: layout.W}
		cs = align.Begin(ops, cs)
		cs.Width.Min = cs.Width.Max
		dims = p.msgEdit.Layout(c, q, ops, cs)
		dims = align.End(dims)
		dims = bg.End(dims)
	}
	c1 := f.End(dims)
	return f.Layout(c1, c2)
}

func (p *threadPage) message(c ui.Config, ops *ui.Ops, cs layout.Constraints, index int) layout.Dimens {
	msg := p.messages[index]
	var dims layout.Dimens
	in := layout.Inset{Top: ui.Dp(8), Left: ui.Dp(8), Right: ui.Dp(40)}
	align := layout.Align{Alignment: layout.W}
	msgMat := colorMaterial(ops, rgb(0xffffff))
	bgcol := theme.brand
	timecol := argb(0xaaaaaaaa)
	if msg.Own {
		in.Left, in.Right = in.Right, in.Left
		align.Alignment = layout.E
		bgcol = colorMaterial(ops, rgb(0xeeeeee))
		msgMat = theme.text
		timecol = rgb(0x888888)
	}
	cs = in.Begin(c, ops, cs)
	{
		cs = align.Begin(ops, cs)
		bg := Background{
			Material: bgcol,
			Inset:    layout.Inset{Top: ui.Dp(8), Bottom: ui.Dp(8), Left: ui.Dp(12), Right: ui.Dp(12)},
			Radius:   ui.Dp(10),
		}
		cs = bg.Begin(c, ops, cs)
		f := layout.Flex{Axis: layout.Vertical, MainAxisAlignment: layout.Start, CrossAxisAlignment: layout.Start}
		f.Init(ops, cs)

		cs = f.Rigid()
		label := text.Label{Material: msgMat, Face: p.env.faces.For(fonts.regular, ui.Sp(14)), Text: msg.Message}
		dims = label.Layout(ops, cs)
		dims.Size.Y += c.Px(ui.Dp(4))
		msgWidth := dims.Size.X
		c1 := f.End(dims)

		cs = f.Rigid()
		{
			cs.Width.Min = msgWidth
			f := layout.Flex{Axis: layout.Horizontal, MainAxisAlignment: layout.SpaceBetween, CrossAxisAlignment: layout.Center}
			f.Init(ops, cs)

			cs = f.Rigid()
			time := formatTime(msg.Time)
			tlbl := text.Label{Material: colorMaterial(ops, timecol), Face: p.env.faces.For(fonts.regular, ui.Sp(10)), Text: time}
			children := []layout.FlexChild{f.End(tlbl.Layout(ops, cs))}

			if msg.Own {
				cs = f.Rigid()
				in := layout.Inset{Left: ui.Dp(12)}
				cs = in.Begin(c, ops, cs)
				checkmark := p.checkmark.image(c, timecol)
				r := checkmark.Bounds()
				if msg.Sent {
					gdraw.ImageOp{Src: checkmark, Rect: r}.Add(ops)
					gdraw.DrawOp{Rect: toRectF(r)}.Add(ops)
				}
				dims = layout.Dimens{Size: r.Size()}
				c := f.End(in.End(dims))
				children = append(children, c)
			}
			dims = f.Layout(children...)
		}
		c2 := f.End(dims)

		dims = f.Layout(c1, c2)
		dims = bg.End(dims)
		dims = align.End(dims)
	}
	return in.End(dims)
}

func (p *threadPage) fetchMessages() {
	p.thread = p.env.client.Thread(p.thread.ID)
	go func() {
		messages, err := p.env.client.Messages(p.thread.ID)
		if err != nil {
			log.Printf("scatter: failed to load messages: %v", err)
			return
		}
		p.result <- messages
		p.env.client.MarkRead(p.thread.ID)
		p.env.redraw()
	}()
}

// formatTime formats a time relative to now. For times within a
// week the date is left out.
func formatTime(t time.Time) string {
	y, m, d := t.Date()
	tday := time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
	y, m, d = time.Now().Date()
	nday := time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
	n := int(nday.Sub(tday) / (time.Hour * 24))
	format := "Jan _2 15:04"
	if n < 7 {
		format = "Mon 15:04"
	}
	return t.Format(format)
}

func (b *IconButton) Events(queue input.Queue) []gesture.ClickEvent {
	return b.click.Events(queue)
}

func (b *IconButton) Layout(c ui.Config, env *Env, ops *ui.Ops, cs layout.Constraints) layout.Dimens {
	ico := b.Icon.image(c, rgb(0xffffff))
	bg := Background{
		Material: theme.brand,
		Radius:   ui.Px(float32(math.Inf(+1))),
		Inset:    b.Inset,
	}
	cs = bg.Begin(c, ops, cs)
	sz := image.Point{X: ico.Bounds().Dx(), Y: ico.Bounds().Dy()}
	cs = layout.RigidConstraints(cs.Constrain(sz))
	dims := widget.Image{Src: ico, Rect: ico.Bounds(), Scale: 1}.Layout(c, ops, cs)
	dims = bg.End(dims)
	pointer.EllipseAreaOp{Size: dims.Size}.Add(ops)
	b.click.Add(ops)
	return dims
}

func (a *App) update(c ui.Config, q input.Queue) {
	page := a.stack.Current()
	if e := page.Event(c, q); e != nil {
		switch e := e.(type) {
		case BackEvent:
			a.stack.Pop()
		case SignInEvent:
			a.env.client.SetAccount(e.Account)
			a.stack.Clear(newThreadsPage(&a.env))
		case NewThreadEvent:
			a.stack.Pop()
			a.stack.Push(newThreadPage(&a.env, e.Address))
		case ShowContactsEvent:
			a.stack.Push(newContactsPage(&a.env))
		case ShowThreadEvent:
			a.stack.Push(newThreadPage(&a.env, e.Thread))
		}
	}
}

type fill struct {
	material ui.MacroOp
}

func (f fill) Layout(ops *ui.Ops, cs layout.Constraints) layout.Dimens {
	d := image.Point{X: cs.Width.Max, Y: cs.Height.Max}
	if d.X == ui.Inf {
		d.X = cs.Width.Min
	}
	if d.Y == ui.Inf {
		d.Y = cs.Height.Min
	}
	dr := f32.Rectangle{
		Max: f32.Point{X: float32(d.X), Y: float32(d.Y)},
	}
	f.material.Add(ops)
	gdraw.DrawOp{Rect: dr}.Add(ops)
	return layout.Dimens{Size: d, Baseline: d.Y}
}

func column() layout.Flex {
	return layout.Flex{Axis: layout.Vertical, MainAxisAlignment: layout.Start, CrossAxisAlignment: layout.Start}
}

func centerRowOpts() layout.Flex {
	return layout.Flex{Axis: layout.Horizontal, MainAxisAlignment: layout.Start, CrossAxisAlignment: layout.Center}
}

func baseline() layout.Flex {
	return layout.Flex{Axis: layout.Horizontal, CrossAxisAlignment: layout.Baseline}
}

type clipCircle struct {
	ops   *ui.Ops
	macro ui.MacroOp
}

func (c *clipCircle) Begin(ops *ui.Ops, cs layout.Constraints) layout.Constraints {
	c.ops = ops
	c.macro.Record(ops)
	return cs
}

func (c *clipCircle) End(dims layout.Dimens) layout.Dimens {
	ops := c.ops
	c.macro.Stop()
	max := dims.Size.X
	if dy := dims.Size.Y; dy > max {
		max = dy
	}
	szf := float32(max)
	rr := szf * .5
	var stack ui.StackOp
	stack.Push(ops)
	rrect(ops, szf, szf, rr, rr, rr, rr)
	c.macro.Add(ops)
	stack.Pop()
	return dims
}

func toRectF(r image.Rectangle) f32.Rectangle {
	return f32.Rectangle{
		Min: f32.Point{X: float32(r.Min.X), Y: float32(r.Min.Y)},
		Max: f32.Point{X: float32(r.Max.X), Y: float32(r.Max.Y)},
	}
}

func (ic *icon) image(c ui.Config, col color.RGBA) image.Image {
	sz := c.Px(ic.size)
	if sz == ic.imgSize {
		return ic.img
	}
	m, _ := iconvg.DecodeMetadata(ic.src)
	dx, dy := m.ViewBox.AspectRatio()
	img := image.NewRGBA(image.Rectangle{Max: image.Point{X: sz, Y: int(float32(sz) * dy / dx)}})
	var ico iconvg.Rasterizer
	ico.SetDstImage(img, img.Bounds(), draw.Src)
	m.Palette[0] = col
	iconvg.Decode(&ico, ic.src, &iconvg.DecodeOptions{
		Palette: &m.Palette,
	})
	ic.img = img
	ic.imgSize = sz
	return img
}

// https://pomax.github.io/bezierinfo/#circles_cubic.
func rrect(ops *ui.Ops, width, height, se, sw, nw, ne float32) {
	w, h := float32(width), float32(height)
	const c = 0.55228475 // 4*(sqrt(2)-1)/3
	var b gdraw.PathBuilder
	b.Init(ops)
	b.Move(f32.Point{X: w, Y: h - se})
	b.Cube(f32.Point{X: 0, Y: se * c}, f32.Point{X: -se + se*c, Y: se}, f32.Point{X: -se, Y: se}) // SE
	b.Line(f32.Point{X: sw - w + se, Y: 0})
	b.Cube(f32.Point{X: -sw * c, Y: 0}, f32.Point{X: -sw, Y: -sw + sw*c}, f32.Point{X: -sw, Y: -sw}) // SW
	b.Line(f32.Point{X: 0, Y: nw - h + sw})
	b.Cube(f32.Point{X: 0, Y: -nw * c}, f32.Point{X: nw - nw*c, Y: -nw}, f32.Point{X: nw, Y: -nw}) // NW
	b.Line(f32.Point{X: w - ne - nw, Y: 0})
	b.Cube(f32.Point{X: ne * c, Y: 0}, f32.Point{X: ne, Y: ne - ne*c}, f32.Point{X: ne, Y: ne}) // NE
	b.End()
}
