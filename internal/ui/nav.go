package ui

import "github.com/nfelddav/logview/internal/query"

type Pos = query.Pos

const farAway = int64(1) << 62

// visible reports whether line p is shown. Without a filter every line is,
// except that a run of lines lost to truncation collapses into its first.
func (a *App) visible(p Pos) bool {
	if p.G < 0 || p.G >= a.seq.NumSegs() || p.I < 0 {
		return false
	}
	if a.filter != nil {
		return a.filter.Visible(p.G, p.I)
	}
	if p.I >= a.seq.DisplayLen(p.G) {
		return false
	}
	lost := a.seq.Info(p.G).Lost
	return !(p.I > 0 && p.I < lost)
}

// nextVis returns the first visible line strictly after p.
func (a *App) nextVis(p Pos) (Pos, bool) {
	g, i := p.G, p.I+1
	if g < 0 {
		g, i = 0, 0
	}
	for n := a.seq.NumSegs(); g < n; g, i = g+1, 0 {
		if a.filter != nil {
			if j := a.filter.Next(g, i); j >= 0 {
				return Pos{G: g, I: j}, true
			}
			continue
		}
		if lost := a.seq.Info(g).Lost; i > 0 && i < lost {
			i = lost
		}
		if i < a.seq.DisplayLen(g) {
			return Pos{G: g, I: i}, true
		}
	}
	return p, false
}

// prevVis returns the last visible line strictly before p.
func (a *App) prevVis(p Pos) (Pos, bool) {
	g, i := p.G, p.I-1
	if n := a.seq.NumSegs(); g >= n {
		g, i = n-1, farAway
	}
	for ; g >= 0; g, i = g-1, farAway {
		if i < 0 {
			continue
		}
		if a.filter != nil {
			if j := a.filter.Prev(g, i); j >= 0 {
				return Pos{G: g, I: j}, true
			}
			continue
		}
		if n := a.seq.DisplayLen(g); i >= n {
			i = n - 1
		}
		if lost := a.seq.Info(g).Lost; i > 0 && i < lost {
			i = 0
		}
		if i >= 0 {
			return Pos{G: g, I: i}, true
		}
	}
	return p, false
}

func (a *App) first() (Pos, bool) { return a.nextVis(Pos{G: 0, I: -1}) }
func (a *App) last() (Pos, bool)  { return a.prevVis(Pos{G: a.seq.NumSegs(), I: 0}) }

func (a *App) atOrAfter(p Pos) (Pos, bool) {
	if a.visible(p) {
		return p, true
	}
	return a.nextVis(p)
}

func (a *App) atOrBefore(p Pos) (Pos, bool) {
	if a.visible(p) {
		return p, true
	}
	return a.prevVis(p)
}

// height is the number of screen rows line p occupies.
func (a *App) height(p Pos) int { return len(a.screenRows(p)) }

// ensureCursor keeps the cursor on a visible line.
func (a *App) ensureCursor() {
	switch {
	case a.follow:
		if p, ok := a.last(); ok {
			a.cursor = p
		}
		a.anchor = nil
	case a.anchor != nil:
		if p, ok := a.atOrAfter(*a.anchor); ok {
			a.cursor = p
		} else if p, ok := a.atOrBefore(*a.anchor); ok {
			a.cursor = p
		}
		if a.filter == nil || a.filter.Done() {
			a.anchor = nil
		}
	case !a.visible(a.cursor):
		if p, ok := a.atOrAfter(a.cursor); ok {
			a.cursor = p
		} else if p, ok := a.atOrBefore(a.cursor); ok {
			a.cursor = p
		}
	}
}

// ensureVisible scrolls minimally so the cursor line is on screen.
func (a *App) ensureVisible() {
	H := a.viewH
	if H <= 0 || !a.visible(a.cursor) {
		return
	}
	if a.follow || !a.visible(a.top) {
		a.scrollToShowBottom(a.cursor)
		return
	}
	if a.cursor.Less(a.top) || (a.cursor == a.top && a.topSkip > 0) {
		a.top, a.topSkip = a.cursor, 0
		return
	}
	acc := -a.topSkip
	for q := a.top; q != a.cursor; {
		acc += a.height(q)
		if acc >= H {
			a.scrollToShowBottom(a.cursor)
			return
		}
		var ok bool
		if q, ok = a.nextVis(q); !ok {
			return
		}
	}
	if acc+a.height(a.cursor) > H {
		a.scrollToShowBottom(a.cursor)
	}
}

// scrollToShowBottom places line p at the bottom of the screen.
func (a *App) scrollToShowBottom(p Pos) {
	H := a.viewH
	h := a.height(p)
	if h >= H {
		a.top, a.topSkip = p, 0
		return
	}
	acc, q := h, p
	for {
		pq, ok := a.prevVis(q)
		if !ok {
			break
		}
		hq := a.height(pq)
		if acc+hq > H {
			a.top, a.topSkip = pq, hq-(H-acc)
			return
		}
		acc += hq
		q = pq
	}
	a.top, a.topSkip = q, 0
}

// scrollRows moves the viewport by n screen rows (negative: up) and puts
// the cursor on the new top line.
func (a *App) scrollRows(n int) {
	q, skip := a.top, a.topSkip
	if !a.visible(q) {
		return
	}
	if n > 0 {
		for n > 0 {
			h := a.height(q) - skip
			if h > n {
				skip += n
				break
			}
			n -= h
			nq, ok := a.nextVis(q)
			if !ok {
				skip = 0
				break
			}
			q, skip = nq, 0
		}
		a.top, a.topSkip = q, skip
		a.cursor = q
		if skip > 0 {
			if nq, ok := a.nextVis(q); ok {
				a.cursor = nq
			}
		}
		return
	}
	n = -n
	if skip >= n {
		a.top, a.topSkip, a.cursor = q, skip-n, q
		return
	}
	n -= skip
	skip = 0
	for n > 0 {
		pq, ok := a.prevVis(q)
		if !ok {
			break
		}
		h := a.height(pq)
		q = pq
		if h >= n {
			skip = h - n
			break
		}
		n -= h
	}
	a.top, a.topSkip, a.cursor = q, skip, q
}

func (a *App) moveCursor(n int) {
	for ; n > 0; n-- {
		p, ok := a.nextVis(a.cursor)
		if !ok {
			break
		}
		a.cursor = p
	}
	for ; n < 0; n++ {
		p, ok := a.prevVis(a.cursor)
		if !ok {
			break
		}
		a.cursor = p
	}
}

// jumpSegment moves to the first line of the next (dir>0) or current/previous segment.
func (a *App) jumpSegment(dir int) {
	g := a.cursor.G
	if dir > 0 {
		for g++; g < a.seq.NumSegs(); g++ {
			if p, ok := a.atOrAfter(Pos{G: g, I: 0}); ok && p.G == g {
				a.cursor = p
				return
			}
		}
		return
	}
	if p, ok := a.atOrAfter(Pos{G: g, I: 0}); ok && p.Less(a.cursor) {
		a.cursor = p
		return
	}
	for g--; g >= 0; g-- {
		if p, ok := a.atOrAfter(Pos{G: g, I: 0}); ok && p.G == g {
			a.cursor = p
			return
		}
	}
}
