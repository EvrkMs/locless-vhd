package main

import (
	"fmt"
	"time"
)

// The session of a player lands in the differencing disk on the store. When the
// store runs full, Windows can no longer write and goes down; the next boot is
// clean again, but the game is lost. The reset service watches the free space
// and tells the player before that happens.
const (
	spaceWarn     = 5 << 30         // below this the player is told once
	spaceCritical = 2 << 30         // below this the player is told again and again
	spaceRearm    = 6 << 30         // above this the warning may come again
	criticalEvery = 5 * time.Minute // how often the critical message repeats
	spaceCheck    = 30 * time.Second
)

// spaceWatch decides when the free space of the store is worth a message.
type spaceWatch struct {
	warned       bool      // the first warning was shown since the space was last fine
	lastCritical time.Time // when the critical message was shown last
}

// check returns the message to show for this much free space, or "".
func (s *spaceWatch) check(free uint64, now time.Time) string {
	switch {
	case free >= spaceRearm:
		s.warned, s.lastCritical = false, time.Time{}
		return ""
	case free < spaceCritical:
		if !s.lastCritical.IsZero() && now.Sub(s.lastCritical) < criticalEvery {
			return ""
		}
		s.warned, s.lastCritical = true, now
		return fmt.Sprintf("Для этого сеанса осталось %s места. Сохраните игру и перезагрузите компьютер: "+
			"после перезагрузки место освободится. Когда место кончится, Windows перезагрузится сама.", formatBytes(free))
	case free < spaceWarn && !s.warned:
		s.warned = true
		return fmt.Sprintf("На компьютере заканчивается место для этого сеанса: осталось %s. "+
			"Сохраните игру, скоро может понадобиться перезагрузка.", formatBytes(free))
	}
	return ""
}

// spaceTitle is the caption of the message window.
const spaceTitle = "Мало места на компьютере"
