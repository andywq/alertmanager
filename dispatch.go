package main

import (
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/prometheus/common/log"
	"github.com/prometheus/common/model"
	"golang.org/x/net/context"

	"github.com/prometheus/alertmanager/notify"
	"github.com/prometheus/alertmanager/provider"
	"github.com/prometheus/alertmanager/types"
)

// Dispatcher sorts incoming alerts into aggregation groups and
// assigns the correct notifiers to each.
type Dispatcher struct {
	route    *Route
	alerts   provider.Alerts
	notifier notify.Notifier

	marker types.Marker

	aggrGroups map[*Route]map[model.Fingerprint]*aggrGroup
	mtx        sync.RWMutex

	done   chan struct{}
	ctx    context.Context
	cancel func()

	log log.Logger
}

// NewDispatcher returns a new Dispatcher.
func NewDispatcher(ap provider.Alerts, r *Route, n notify.Notifier, mk types.Marker) *Dispatcher {
	disp := &Dispatcher{
		alerts:   ap,
		notifier: n,
		route:    r,
		marker:   mk,
		log:      log.With("component", "dispatcher"),
	}
	return disp
}

// Run starts dispatching alerts incoming via the updates channel.
func (d *Dispatcher) Run() {
	d.done = make(chan struct{})

	d.mtx.Lock()
	d.aggrGroups = map[*Route]map[model.Fingerprint]*aggrGroup{}
	d.mtx.Unlock()

	d.ctx, d.cancel = context.WithCancel(context.Background())

	d.run(d.alerts.Subscribe())
	close(d.done)
}

// AlertBlock contains a list of alerts associated with a set of
// routing options.
type AlertBlock struct {
	RouteOpts *RouteOpts  `json:"routeOpts"`
	Alerts    []*APIAlert `json:"alerts"`
}

// APIAlert is the API representation of an alert, which is a regular alert
// annotated with silencing and inhibition info.
type APIAlert struct {
	*types.Alert

	Inhibited bool   `json:"inhibited"`
	Silenced  uint64 `json:"silenced,omitempty"`
}

// AlertGroup is a list of alert blocks grouped by the same label set.
type AlertGroup struct {
	Labels model.LabelSet `json:"labels"`
	Blocks []*AlertBlock  `json:"blocks"`
}

// AlertOverview is a representation of all active alerts in the system.
type AlertOverview []*AlertGroup

func (ao AlertOverview) Swap(i, j int)      { ao[i], ao[j] = ao[j], ao[i] }
func (ao AlertOverview) Less(i, j int) bool { return ao[i].Labels.Before(ao[j].Labels) }
func (ao AlertOverview) Len() int           { return len(ao) }

// Groups populates an AlertOverview from the dispatcher's internal state.
func (d *Dispatcher) Groups() AlertOverview {
	var overview AlertOverview

	d.mtx.RLock()
	defer d.mtx.RUnlock()

	seen := map[model.Fingerprint]*AlertGroup{}

	for route, ags := range d.aggrGroups {
		for _, ag := range ags {
			alertGroup, ok := seen[ag.fingerprint()]
			if !ok {
				alertGroup = &AlertGroup{Labels: ag.labels}

				seen[ag.fingerprint()] = alertGroup
				overview = append(overview, alertGroup)
			}

			now := time.Now()

			var apiAlerts []*APIAlert
			for _, a := range ag.alertSlice() {
				if !a.EndsAt.IsZero() && a.EndsAt.Before(now) {
					continue
				}

				sid, _ := d.marker.Silenced(a.Fingerprint())

				apiAlerts = append(apiAlerts, &APIAlert{
					Alert:     a,
					Inhibited: d.marker.Inhibited(a.Fingerprint()),
					Silenced:  sid,
				})
			}
			if len(apiAlerts) == 0 {
				continue
			}

			alertGroup.Blocks = append(alertGroup.Blocks, &AlertBlock{
				RouteOpts: &route.RouteOpts,
				Alerts:    apiAlerts,
			})
		}
	}

	sort.Sort(overview)

	return overview
}

func (d *Dispatcher) run(it provider.AlertIterator) {
	cleanup := time.NewTicker(30 * time.Second)
	defer cleanup.Stop()

	defer it.Close()

	for {
		select {
		case alert, ok := <-it.Next():
			if !ok {
				// Iterator exhausted for some reason.
				if err := it.Err(); err != nil {
					log.Errorf("Error on alert update: %s", err)
				}
				return
			}

			d.log.With("alert", alert).Debug("Received alert")

			// Log errors but keep trying.
			if err := it.Err(); err != nil {
				log.Errorf("Error on alert update: %s", err)
				continue
			}

			for _, r := range d.route.Match(alert.Labels) {
				d.processAlert(alert, r)
			}

		case <-cleanup.C:
			d.mtx.Lock()

			for _, groups := range d.aggrGroups {
				for _, ag := range groups {
					if ag.empty() {
						ag.stop()
						delete(groups, ag.fingerprint())
					}
				}
			}

			d.mtx.Unlock()

		case <-d.ctx.Done():
			return
		}
	}
}

// Stop the dispatcher.
func (d *Dispatcher) Stop() {
	if d == nil || d.cancel == nil {
		return
	}
	d.cancel()
	d.cancel = nil

	<-d.done
}

// notifyFunc is a function that performs notifcation for the alert
// with the given fingerprint. It aborts on context cancelation.
// Returns false iff notifying failed.
type notifyFunc func(context.Context, ...*types.Alert) bool

// processAlert determines in which aggregation group the alert falls
// and insert it.
func (d *Dispatcher) processAlert(alert *types.Alert, route *Route) {
	group := model.LabelSet{}

	for ln, lv := range alert.Labels {
		if _, ok := route.RouteOpts.GroupBy[ln]; ok {
			group[ln] = lv
		}
	}

	fp := group.Fingerprint()

	d.mtx.Lock()
	groups, ok := d.aggrGroups[route]
	if !ok {
		groups = map[model.Fingerprint]*aggrGroup{}
		d.aggrGroups[route] = groups
	}
	d.mtx.Unlock()

	// If the group does not exist, create it.
	ag, ok := groups[fp]
	if !ok {
		ag = newAggrGroup(d.ctx, group, &route.RouteOpts)
		groups[fp] = ag

		go ag.run(func(ctx context.Context, alerts ...*types.Alert) bool {
			err := d.notifier.Notify(ctx, alerts...)
			if err != nil {
				log.Errorf("Notify for %d alerts failed: %s", len(alerts), err)
			}
			return err == nil
		})
	}

	ag.insert(alert)
}

// aggrGroup aggregates alert fingerprints into groups to which a
// common set of routing options applies.
// It emits notifications in the specified intervals.
type aggrGroup struct {
	labels  model.LabelSet
	opts    *RouteOpts
	routeFP model.Fingerprint
	log     log.Logger

	ctx    context.Context
	cancel func()
	done   chan struct{}
	next   *time.Timer

	mtx     sync.RWMutex
	alerts  map[model.Fingerprint]*types.Alert
	hasSent bool
}

// newAggrGroup returns a new aggregation group.
func newAggrGroup(ctx context.Context, labels model.LabelSet, opts *RouteOpts) *aggrGroup {
	ag := &aggrGroup{
		labels: labels,
		opts:   opts,
		alerts: map[model.Fingerprint]*types.Alert{},
	}
	ag.ctx, ag.cancel = context.WithCancel(ctx)

	ag.log = log.With("aggrGroup", ag)

	// Set an initial one-time wait before flushing
	// the first batch of notifications.
	ag.next = time.NewTimer(ag.opts.GroupWait)

	return ag
}

func (ag *aggrGroup) String() string {
	return fmt.Sprint(ag.fingerprint())
}

func (ag *aggrGroup) alertSlice() []*types.Alert {
	ag.mtx.RLock()
	defer ag.mtx.RUnlock()

	var alerts []*types.Alert
	for _, a := range ag.alerts {
		alerts = append(alerts, a)
	}
	return alerts
}

func (ag *aggrGroup) run(nf notifyFunc) {
	ag.done = make(chan struct{})

	defer close(ag.done)
	defer ag.next.Stop()

	timeout := ag.opts.GroupInterval

	if timeout < notify.MinTimeout {
		timeout = notify.MinTimeout
	}

	for {
		select {
		case now := <-ag.next.C:
			// Give the notifcations time until the next flush to
			// finish before terminating them.
			ctx, cancel := context.WithTimeout(ag.ctx, timeout)

			// The now time we retrieve from the ticker is the only reliable
			// point of time reference for the subsequent notification pipeline.
			// Calculating the current time directly is prone to flaky behavior,
			// which usually only becomes apparent in tests.
			ctx = notify.WithNow(ctx, now)

			// Populate context with information needed along the pipeline.
			ctx = notify.WithGroupKey(ctx, ag.labels.Fingerprint()^ag.routeFP)
			ctx = notify.WithGroupLabels(ctx, ag.labels)
			ctx = notify.WithReceiver(ctx, ag.opts.Receiver)
			ctx = notify.WithRepeatInterval(ctx, ag.opts.RepeatInterval)

			// Wait the configured interval before calling flush again.
			ag.mtx.Lock()
			ag.next.Reset(ag.opts.GroupInterval)
			ag.mtx.Unlock()

			ag.flush(func(alerts ...*types.Alert) bool {
				return nf(ctx, alerts...)
			})

			cancel()

		case <-ag.ctx.Done():
			return
		}
	}
}

func (ag *aggrGroup) stop() {
	// Calling cancel will terminate all in-process notifications
	// and the run() loop.
	ag.cancel()
	<-ag.done
}

func (ag *aggrGroup) fingerprint() model.Fingerprint {
	return ag.labels.Fingerprint()
}

// insert inserts the alert into the aggregation group. If the aggregation group
// is empty afterwards, it returns true.
func (ag *aggrGroup) insert(alert *types.Alert) {
	ag.mtx.Lock()
	defer ag.mtx.Unlock()

	ag.alerts[alert.Fingerprint()] = alert

	// Immediately trigger a flush if the wait duration for this
	// alert is already over.
	if !ag.hasSent && alert.StartsAt.Add(ag.opts.GroupWait).Before(time.Now()) {
		ag.next.Reset(0)
	}
}

func (ag *aggrGroup) empty() bool {
	ag.mtx.RLock()
	defer ag.mtx.RUnlock()

	return len(ag.alerts) == 0
}

// flush sends notifications for all new alerts.
func (ag *aggrGroup) flush(notify func(...*types.Alert) bool) {
	if ag.empty() {
		return
	}
	ag.mtx.Lock()

	var (
		alerts      = make(map[model.Fingerprint]*types.Alert, len(ag.alerts))
		alertsSlice = make([]*types.Alert, 0, len(ag.alerts))
	)
	for fp, alert := range ag.alerts {
		alerts[fp] = alert
		alertsSlice = append(alertsSlice, alert)
	}

	ag.mtx.Unlock()

	ag.log.Debugln("flushing", alertsSlice)

	if notify(alertsSlice...) {
		ag.mtx.Lock()
		for fp, a := range alerts {
			// Only delete if the fingerprint has not been inserted
			// again since we notified about it.
			if a.Resolved() && ag.alerts[fp] == a {
				delete(ag.alerts, fp)
			}
		}

		ag.hasSent = true
		ag.mtx.Unlock()
	}
}
