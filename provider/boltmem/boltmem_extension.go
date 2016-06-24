package boltmem

import (
	"encoding/binary"
	"encoding/json"
	"path/filepath"

	"github.com/boltdb/bolt"
	"github.com/prometheus/alertmanager/provider"
	"github.com/prometheus/alertmanager/types"
)

var bktEvents = []byte("events")

type Events struct {
	db *bolt.DB
}

func NewEvents(path string) (*Events, error) {
	db, err := bolt.Open(filepath.Join(path, "events.db"), 0666, nil)
	if err != nil {
		return nil, err
	}
	err = db.Update(func(tx *bolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists(bktEvents)
		return err
	})
	return &Events{db: db}, err
}

func (s *Events) Set(event *types.Event) (uint64, error) {
	var (
		uid uint64
		err error
	)
	err = s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bktEvents)

		uid, err = b.NextSequence()
		if err != nil {
			return err
		}
		event.ID = uid

		k := make([]byte, 8)
		binary.BigEndian.PutUint64(k, uid)

		msb, err := json.Marshal(event)
		if err != nil {
			return err
		}
		return b.Put(k, msb)
	})
	return uid, err
}

// All returns all existing events.
func (s *Events) All() ([]*types.Event, error) {
	var res []*types.Event

	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bktEvents)
		c := b.Cursor()

		for k, v := c.First(); k != nil; k, v = c.Next() {
			var ms types.Event
			if err := json.Unmarshal(v, &ms); err != nil {
				return err
			}
			ms.ID = binary.BigEndian.Uint64(k)
			res = append(res, &ms)
		}

		return nil
	})

	return res, err
}

func (a *Events) Get(id uint64) (*types.Event, error) {
	var event types.Event
	err := a.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bktEvents)

		k := make([]byte, 8)
		binary.BigEndian.PutUint64(k, id)

		ab := b.Get(k)
		if ab == nil {
			return provider.ErrNotFound
		}

		return json.Unmarshal(ab, &event)
	})
	return &event, err
}

func (s *Events) Close() error {
	return s.db.Close()
}
