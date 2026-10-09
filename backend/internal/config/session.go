package config

import (
	"fmt"
	"time"

	"stockbot/backend/internal/domain/session"
)

// SessionHours is the YAML form of the venue calendar.
type SessionHours struct {
	Timezone string `yaml:"timezone"`
	Sessions []struct {
		Start string `yaml:"start"`
		End   string `yaml:"end"`
	} `yaml:"sessions"`
	EntryCutoff string   `yaml:"entry_cutoff"`
	ForceFlatAt string   `yaml:"force_flat_at"`
	Holidays    []string `yaml:"holidays"` // "YYYY-MM-DD" venue closures (祝日・年末年始)
	// CalendarThrough is the last date the holidays list is known to cover. Beyond it
	// the runtime fails closed until a human extends the calendar — an un-updated 祝日
	// list must never trade blind through the next year(年次更新)。Empty = no horizon.
	CalendarThrough string `yaml:"calendar_through"`
}

// TradingHours converts the YAML form to the domain value. Defaults to Asia/Tokyo.
func (s SessionHours) TradingHours() (session.TradingHours, error) {
	tzName := s.Timezone
	if tzName == "" {
		tzName = "Asia/Tokyo"
	}
	loc, err := time.LoadLocation(tzName)
	if err != nil {
		return session.TradingHours{}, fmt.Errorf("load timezone %q: %w", tzName, err)
	}
	windows := make([]session.Window, 0, len(s.Sessions))
	for _, w := range s.Sessions {
		windows = append(windows, session.Window{Start: w.Start, End: w.End})
	}
	holidays := make(map[string]struct{}, len(s.Holidays))
	for _, d := range s.Holidays {
		if _, err := time.ParseInLocation("2006-01-02", d, loc); err != nil {
			return session.TradingHours{}, fmt.Errorf("holiday %q must be YYYY-MM-DD: %w", d, err)
		}
		holidays[d] = struct{}{}
	}
	var through time.Time
	if s.CalendarThrough != "" {
		through, err = time.ParseInLocation("2006-01-02", s.CalendarThrough, loc)
		if err != nil {
			return session.TradingHours{}, fmt.Errorf("calendar_through %q must be YYYY-MM-DD: %w", s.CalendarThrough, err)
		}
	}
	return session.TradingHours{
		TZ:              loc,
		Sessions:        windows,
		EntryCutoff:     s.EntryCutoff,
		ForceFlatAt:     s.ForceFlatAt,
		Holidays:        holidays,
		CalendarThrough: through,
	}, nil
}
