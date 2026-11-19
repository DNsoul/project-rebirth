package lifecycle

import (
	"context"
	"time"

	"awgrotator/internal/usage"
)

// HeavyDevice is how much one device may move in a day before the operator is
// told: about what a whole 1 TB monthly allowance gives for a day.
const HeavyDevice = 30e9

// DefaultHeavyEvery is how often the node is asked who uses the most.
const DefaultHeavyEvery = 30 * time.Minute

// HeavyUsage is one device that went over HeavyDevice in the last day.
type HeavyUsage struct {
	ID, Name, Code string
	Bytes          uint64 // the device, both ways
	Total          uint64 // every device together
}

// maybeHeavy tells the operator, once a day per device and in their daytime,
// about a device that has used more than HeavyDevice in the last 24 hours.
func (m *Manager) maybeHeavy(ctx context.Context, now time.Time) {
	zone := m.Cfg.OwnerZone()
	if !m.current || Quiet(now, zone) {
		return
	}
	if !m.lastHeavy.IsZero() && now.Sub(m.lastHeavy) < m.Every.Heavy {
		return
	}
	m.lastHeavy = now
	r, err := m.Host.Usage(ctx, usage.Day)
	if err != nil || r == nil {
		m.log().Debug("could not ask the node where the traffic went", "error", err)
		return
	}
	day := now.In(zone).Format("2006-01-02")
	for _, d := range r.Devices {
		if d.Total() < HeavyDevice {
			break // largest first
		}
		key := "heavy:" + m.Cfg.InstanceID + ":" + d.Key + ":" + day
		if m.J.Read().Noticed(key) {
			continue
		}
		if err := m.J.Update(func(j *Journal) { j.MarkNoticed(key) }); err != nil {
			m.log().Warn("could not record a traffic notice; not sending it", "error", err)
			return
		}
		label := r.Names[d.Key]
		m.log().Info("telling the operator about a device that uses a lot", "gigabytes", d.Total()/1e9)
		m.emit(Event{Kind: EventHeavy, Heavy: &HeavyUsage{
			ID: d.Key, Name: label.Name, Code: label.Code, Bytes: d.Total(), Total: r.Total.Total(),
		}})
	}
}
