package main

import (
	"time"
	"strconv"
	"net/http"

	"github.com/prometheus/common/model"
	"github.com/prometheus/common/route"

	"github.com/prometheus/alertmanager/types"
)

func (api *API) listEvents(w http.ResponseWriter, r *http.Request) {
	events, err := api.events.All()
	if err != nil {
		respondError(w, apiError{
			typ: errorInternal,
			err: err,
		}, nil)
		return
	}
	respond(w, events)
}

func (api *API) listEventAlerts(w http.ResponseWriter, r *http.Request) {
	eids := route.Param(api.context(r), "eid")
	eid, err := strconv.ParseUint(eids, 10, 64)
	if err != nil {
		respondError(w, apiError{
			typ: errorInternal,
			err: err,
		}, nil)
		return
	}

	event, err := api.events.Get(eid)
	if err != nil {
		respondError(w, apiError{
			typ: errorInternal,
			err: err,
		}, nil)
		return
	}

	var alerts []*types.Alert
	for _, ids := range event.Alerts {
		id, err := strconv.ParseUint(ids, 10, 64)
		if err != nil {
			respondError(w, apiError{
				typ: errorInternal,
				err: err,
			}, nil)
			return
		}

		a, err := api.alerts.Get(model.Fingerprint(id))
		if err != nil {
			respondError(w, apiError{
				typ: errorInternal,
				err: err,
			}, nil)
			return
		}
		alerts = append(alerts, a)
	}

	respond(w, alerts)
}

func (api *API) addEvent(w http.ResponseWriter, r *http.Request) {
	var event types.Event
	if err := receive(r, &event); err != nil {
		respondError(w, apiError{
			typ: errorBadData,
			err: err,
		}, nil)
		return
	}

	if event.CreatedAt.IsZero() {
		event.CreatedAt = time.Now()
	}

	sid, err := api.events.Set(&event)
	if err != nil {
		respondError(w, apiError{
			typ: errorInternal,
			err: err,
		}, nil)
		return
	}

	respond(w, struct {
		EventID uint64 `json:"eventId"`
	}{
		EventID: sid,
	})
}
