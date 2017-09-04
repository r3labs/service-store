/* This Source Code Form is subject to the terms of the Mozilla Public
 * License, v. 2.0. If a copy of the MPL was not distributed with this
 * file, You can obtain one at http://mozilla.org/MPL/2.0/. */

package main

import (
	"encoding/json"
	"errors"
	"log"
	"time"

	"github.com/ernestio/service-store/models"
	"github.com/nats-io/nats"
	"github.com/r3labs/natsdb"
)

// ServiceView : renders an old singular service by joining data on builds and services tables
type ServiceView struct {
	ID           uint       `json:"-" gorm:"primary_key"`
	IDs          []string   `json:"ids,omitempty" gorm:"-"`
	Names        []string   `json:"names,omitempty" gorm:"-"`
	UUID         string     `json:"id"`
	UserID       uint       `json:"user_id"`
	DatacenterID uint       `json:"datacenter_id"`
	Name         string     `json:"name"`
	Type         string     `json:"type"`
	Version      time.Time  `json:"version"`
	Status       string     `json:"status"`
	Options      models.Map `json:"options"`
	Credentials  models.Map `json:"credentials"`
	Definition   string     `json:"definition,omitempty"`
	Mapping      models.Map `json:"mapping,omitempty" gorm:"type:text;"`
}

// Find : based on the defined fields for the current entity
// will perform a search on the database
func (s *ServiceView) Find() []interface{} {
	var results []interface{}
	var services []ServiceView

	q := db.Table("environments").Select("builds.id as id, builds.uuid, builds.user_id, builds.status, builds.definition, builds.created_at as version, environments.name, environments.datacenter_id, environments.options, environments.credentials, environments.type").Joins("INNER JOIN builds ON (builds.environment_id = environments.id)")

	if len(s.IDs) > 0 {
		q = q.Where("builds.uuid in (?)", s.IDs)
	} else if len(s.Names) > 0 {
		q = q.Where("environments.name in (?)", s.Names)
	} else if s.Name != "" {
		if s.UUID != "" {
			q = q.Where("environments.name = ?", s.Name).Where("builds.uuid = ?", s.UUID)
		} else {
			q = q.Where("environments.name = ?", s.Name)
		}
	} else {
		if s.UUID != "" {
			q = q.Where("builds.uuid = ?", s.UUID)
		} else if s.Name != "" {
			q = q.Where("environments.name = ?", s.Name)
		} else if s.DatacenterID != 0 {
			q = q.Where("environments.datacenter_id = ?", s.DatacenterID)
		}
	}

	q.Order("version desc").Find(&services)

	results = make([]interface{}, len(services))

	for i := 0; i < len(services); i++ {
		results[i] = &services[i]
	}

	return results
}

// MapInput : maps the input []byte on the current entity
func (s *ServiceView) MapInput(body []byte) {
	if err := json.Unmarshal(body, &s); err != nil {
		log.Println(err)
	}
}

// HasID : determines if the current entity has an id or not
func (s *ServiceView) HasID() bool {
	return s.ID != 0
}

// LoadFromInput : Will load from a []byte input the database stored entity
func (s *ServiceView) LoadFromInput(msg []byte) bool {
	s.MapInput(msg)
	var stored ServiceView

	q := db.Table("environments").Select("builds.id as id, builds.uuid, builds.user_id, builds.status, builds.created_at as version, environments.name, environments.datacenter_id, environments.options, environments.credentials").Joins("INNER JOIN builds ON (builds.environment_id = environments.id)")

	if s.UUID != "" {
		q = q.Where("builds.uuid = ?", s.UUID)
	} else if s.Name != "" {
		q = q.Where("environments.name = ?", s.Name)
	}

	err := q.First(&stored).Error
	if err != nil {
		return false
	}

	if !stored.HasID() {
		return false
	}

	*s = stored

	return true
}

// LoadFromInputOrFail : Will try to load from the input an existing entity,
// or will call the handler to Fail the nats message
func (s *ServiceView) LoadFromInputOrFail(msg *nats.Msg, h *natsdb.Handler) bool {
	stored := &ServiceView{}
	ok := stored.LoadFromInput(msg.Data)
	if !ok {
		h.Fail(msg)
	}
	*s = *stored

	return ok
}

// Update : It will update the current entity with the input []byte
func (s *ServiceView) Update(body []byte) error {
	s.MapInput(body)

	if s.Name == "" {
		return errors.New("service name was not specified")
	}

	var env models.Environment

	db.Where("name = ?", s.Name).First(&env)

	env.Options = s.Options
	env.Credentials = s.Credentials

	return db.Save(&env).Error
}

// Delete : Will delete from database the current ServiceView
func (s *ServiceView) Delete() error {
	var env models.Environment

	err := db.Where("name = ?", s.Name).First(&env).Error
	if err != nil {
		return err
	}

	return env.Delete()
}

// Save : Persists current entity on database
func (s *ServiceView) Save() error {
	var err error

	tx := db.Begin()
	tx.Exec("set transaction isolation level serializable")

	defer func() {
		switch err {
		case nil:
			err = tx.Commit().Error
		default:
			log.Println(err)
			err = tx.Rollback().Error
		}
	}()

	env := models.Environment{
		Name:         s.Name,
		DatacenterID: s.DatacenterID,
		Type:         s.Type,
		Options:      s.Options,
		Credentials:  s.Credentials,
		Status:       "initializing",
	}

	err = tx.Where("name = ?", s.Name).FirstOrCreate(&env).Error
	if err != nil {
		return err
	}

	// if no build properties are sent, just create the service entry
	if s.UUID == "" && s.Type == "" {
		return nil
	}

	switch env.Status {
	case "initializing", "done", "errored":
		err = tx.Exec("UPDATE environments SET status = ? WHERE id = ?", "in_progress", env.ID).Error
	case "in_progress":
		err = errors.New(`{"error": "could not create environment build: service in progress"}`)
	default:
		err = errors.New(`{"error": "could not create environment build: unknown service state"}`)
	}

	if err != nil {
		return err
	}

	build := models.Build{
		UUID:          s.UUID,
		EnvironmentID: env.ID,
		UserID:        s.UserID,
		Type:          s.Type,
		Status:        "in_progress",
		Definition:    s.Definition,
		Mapping:       s.Mapping,
	}

	err = tx.Save(&build).Error
	if err != nil {
		return err
	}

	s.Version = build.CreatedAt
	s.Status = build.Status

	if s.Credentials == nil {
		return nil
	}

	env.Credentials = s.Credentials
	err = tx.Save(&env).Error

	return err
}
