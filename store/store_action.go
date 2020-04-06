// -*- Mode: Go; indent-tabs-mode: t -*-

/*
 * Copyright (C) 2014-2020 Canonical Ltd
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License version 3 as
 * published by the Free Software Foundation.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 *
 */

// Package store has support to use the Ubuntu Store for querying and downloading of snaps, and the related services.
package store

import (
	"context"
	"crypto"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"

	"github.com/snapcore/snapd/jsonutil"
	"github.com/snapcore/snapd/logger"
	"github.com/snapcore/snapd/overlord/auth"
	"github.com/snapcore/snapd/snap"
)

type RefreshOptions struct {
	// RefreshManaged indicates to the store that the refresh is
	// managed via snapd-control.
	RefreshManaged bool
	IsAutoRefresh  bool

	PrivacyKey string
}

// snap action: install/refresh

type CurrentSnap struct {
	InstanceName     string
	SnapID           string
	Revision         snap.Revision
	TrackingChannel  string
	RefreshedDate    time.Time
	IgnoreValidation bool
	Block            []snap.Revision
	Epoch            snap.Epoch
	CohortKey        string
}

type currentSnapV2JSON struct {
	SnapID           string     `json:"snap-id"`
	InstanceKey      string     `json:"instance-key"`
	Revision         int        `json:"revision"`
	TrackingChannel  string     `json:"tracking-channel"`
	Epoch            snap.Epoch `json:"epoch"`
	RefreshedDate    *time.Time `json:"refreshed-date,omitempty"`
	IgnoreValidation bool       `json:"ignore-validation,omitempty"`
	CohortKey        string     `json:"cohort-key,omitempty"`
}

type SnapActionFlags int

const (
	SnapActionIgnoreValidation SnapActionFlags = 1 << iota
	SnapActionEnforceValidation
)

type SnapAction struct {
	Action       string
	InstanceName string
	SnapID       string
	Channel      string
	Revision     snap.Revision
	CohortKey    string
	Flags        SnapActionFlags
	Epoch        snap.Epoch
}

func isValidAction(action string) bool {
	switch action {
	case "download", "install", "refresh":
		return true
	default:
		return false
	}
}

type snapActionJSON struct {
	Action           string `json:"action"`
	InstanceKey      string `json:"instance-key"`
	Name             string `json:"name,omitempty"`
	SnapID           string `json:"snap-id,omitempty"`
	Channel          string `json:"channel,omitempty"`
	Revision         int    `json:"revision,omitempty"`
	CohortKey        string `json:"cohort-key,omitempty"`
	IgnoreValidation *bool  `json:"ignore-validation,omitempty"`

	// NOTE the store needs an epoch (even if null) for the "install" and "download"
	// actions, to know the client handles epochs at all.  "refresh" actions should
	// send nothing, not even null -- the snap in the context should have the epoch
	// already.  We achieve this by making Epoch be an `interface{}` with omitempty,
	// and then setting it to a (possibly nil) epoch for install and download. As a
	// nil epoch is not an empty interface{}, you'll get the null in the json.
	Epoch interface{} `json:"epoch,omitempty"`
}

type snapRelease struct {
	Architecture string `json:"architecture"`
	Channel      string `json:"channel"`
}

type snapActionResult struct {
	Result           string    `json:"result"`
	InstanceKey      string    `json:"instance-key"`
	SnapID           string    `json:"snap-id,omitempy"`
	Name             string    `json:"name,omitempty"`
	Snap             storeSnap `json:"snap"`
	EffectiveChannel string    `json:"effective-channel,omitempty"`
	RedirectChannel  string    `json:"redirect-channel,omitempty"`
	Error            struct {
		Code    string `json:"code"`
		Message string `json:"message"`
		Extra   struct {
			Releases []snapRelease `json:"releases"`
		} `json:"extra"`
	} `json:"error"`
}

type snapActionRequest struct {
	Context []*currentSnapV2JSON `json:"context"`
	Actions []*snapActionJSON    `json:"actions"`
	Fields  []string             `json:"fields"`
}

type snapActionResultList struct {
	Results   []*snapActionResult `json:"results"`
	ErrorList []struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error-list"`
}

var snapActionFields = jsonutil.StructFields((*storeSnap)(nil))

// SnapAction queries the store for snap information for the given
// install/refresh actions, given the context information about
// current installed snaps in currentSnaps. If the request was overall
// successul (200) but there were reported errors it will return both
// the snap infos and an SnapActionError.
func (s *Store) SnapAction(ctx context.Context, currentSnaps []*CurrentSnap, actions []*SnapAction, user *auth.UserState, opts *RefreshOptions) ([]SnapActionResult, error) {
	if opts == nil {
		opts = &RefreshOptions{}
	}

	if len(currentSnaps) == 0 && len(actions) == 0 {
		// nothing to do
		return nil, &SnapActionError{NoResults: true}
	}

	authRefreshes := 0
	for {
		sars, err := s.snapAction(ctx, currentSnaps, actions, user, opts)

		if saErr, ok := err.(*SnapActionError); ok && authRefreshes < 2 && len(saErr.Other) > 0 {
			// do we need to try to refresh auths?, 2 tries
			var refreshNeed authRefreshNeed
			for _, otherErr := range saErr.Other {
				switch otherErr {
				case errUserAuthorizationNeedsRefresh:
					refreshNeed.user = true
				case errDeviceAuthorizationNeedsRefresh:
					refreshNeed.device = true
				}
			}
			if refreshNeed.needed() {
				err := s.refreshAuth(user, refreshNeed)
				if err != nil {
					// best effort
					logger.Noticef("cannot refresh soft-expired authorisation: %v", err)
				}
				authRefreshes++
				// TODO: we could avoid retrying here
				// if refreshAuth gave no error we got
				// as many non-error results from the
				// store as actions anyway
				continue
			}
		}

		return sars, err
	}
}

func genInstanceKey(curSnap *CurrentSnap, salt string) (string, error) {
	_, snapInstanceKey := snap.SplitInstanceName(curSnap.InstanceName)

	if snapInstanceKey == "" {
		return curSnap.SnapID, nil
	}

	if salt == "" {
		return "", fmt.Errorf("internal error: request salt not provided")
	}

	// due to privacy concerns, avoid sending the local names to the
	// backend, instead hash the snap ID and instance key together
	h := crypto.SHA256.New()
	h.Write([]byte(curSnap.SnapID))
	h.Write([]byte(snapInstanceKey))
	h.Write([]byte(salt))
	enc := base64.RawURLEncoding.EncodeToString(h.Sum(nil))
	return fmt.Sprintf("%s:%s", curSnap.SnapID, enc), nil
}

// SnapActionResult encapsulates the non-error result of a single
// action of the SnapAction call.
type SnapActionResult struct {
	*snap.Info
	RedirectChannel string
}

func (s *Store) snapAction(ctx context.Context, currentSnaps []*CurrentSnap, actions []*SnapAction, user *auth.UserState, opts *RefreshOptions) ([]SnapActionResult, error) {

	// TODO: the store already requires instance-key but doesn't
	// yet support repeating in context or sending actions for the
	// same snap-id, for now we keep instance-key handling internal

	requestSalt := ""
	if opts != nil {
		requestSalt = opts.PrivacyKey
	}
	curSnaps := make(map[string]*CurrentSnap, len(currentSnaps))
	curSnapJSONs := make([]*currentSnapV2JSON, len(currentSnaps))
	instanceNameToKey := make(map[string]string, len(currentSnaps))
	for i, curSnap := range currentSnaps {
		if curSnap.SnapID == "" || curSnap.InstanceName == "" || curSnap.Revision.Unset() {
			return nil, fmt.Errorf("internal error: invalid current snap information")
		}
		instanceKey, err := genInstanceKey(curSnap, requestSalt)
		if err != nil {
			return nil, err
		}
		curSnaps[instanceKey] = curSnap
		instanceNameToKey[curSnap.InstanceName] = instanceKey

		channel := curSnap.TrackingChannel
		if channel == "" {
			channel = "stable"
		}
		var refreshedDate *time.Time
		if !curSnap.RefreshedDate.IsZero() {
			refreshedDate = &curSnap.RefreshedDate
		}
		curSnapJSONs[i] = &currentSnapV2JSON{
			SnapID:           curSnap.SnapID,
			InstanceKey:      instanceKey,
			Revision:         curSnap.Revision.N,
			TrackingChannel:  channel,
			IgnoreValidation: curSnap.IgnoreValidation,
			RefreshedDate:    refreshedDate,
			Epoch:            curSnap.Epoch,
			CohortKey:        curSnap.CohortKey,
		}
	}

	downloadNum := 0
	installNum := 0
	installs := make(map[string]*SnapAction, len(actions))
	downloads := make(map[string]*SnapAction, len(actions))
	refreshes := make(map[string]*SnapAction, len(actions))
	actionJSONs := make([]*snapActionJSON, len(actions))
	for i, a := range actions {
		if !isValidAction(a.Action) {
			return nil, fmt.Errorf("internal error: unsupported action %q", a.Action)
		}
		if a.InstanceName == "" {
			return nil, fmt.Errorf("internal error: action without instance name")
		}
		var ignoreValidation *bool
		if a.Flags&SnapActionIgnoreValidation != 0 {
			var t = true
			ignoreValidation = &t
		} else if a.Flags&SnapActionEnforceValidation != 0 {
			var f = false
			ignoreValidation = &f
		}

		var instanceKey string
		aJSON := &snapActionJSON{
			Action:           a.Action,
			SnapID:           a.SnapID,
			Channel:          a.Channel,
			Revision:         a.Revision.N,
			CohortKey:        a.CohortKey,
			IgnoreValidation: ignoreValidation,
		}
		if !a.Revision.Unset() {
			a.Channel = ""
		}

		if a.Action == "install" {
			installNum++
			instanceKey = fmt.Sprintf("install-%d", installNum)
			installs[instanceKey] = a
		} else if a.Action == "download" {
			downloadNum++
			instanceKey = fmt.Sprintf("download-%d", downloadNum)
			downloads[instanceKey] = a
			if _, key := snap.SplitInstanceName(a.InstanceName); key != "" {
				return nil, fmt.Errorf("internal error: unsupported download with instance name %q", a.InstanceName)
			}
		} else {
			instanceKey = instanceNameToKey[a.InstanceName]
			refreshes[instanceKey] = a
		}

		if a.Action != "refresh" {
			aJSON.Name = snap.InstanceSnap(a.InstanceName)
			if a.Epoch.IsZero() {
				// Let the store know we can handle epochs, by sending the `epoch`
				// field in the request.  A nil epoch is not an empty interface{},
				// you'll get the null in the json. See comment in snapActionJSON.
				aJSON.Epoch = (*snap.Epoch)(nil)
			} else {
				// this is the amend case
				aJSON.Epoch = &a.Epoch
			}
		}

		aJSON.InstanceKey = instanceKey

		actionJSONs[i] = aJSON
	}

	// build input for the install/refresh endpoint
	jsonData, err := json.Marshal(snapActionRequest{
		Context: curSnapJSONs,
		Actions: actionJSONs,
		Fields:  snapActionFields,
	})
	if err != nil {
		return nil, err
	}

	reqOptions := &requestOptions{
		Method:      "POST",
		URL:         s.endpointURL(snapActionEndpPath, nil),
		Accept:      jsonContentType,
		ContentType: jsonContentType,
		Data:        jsonData,
		APILevel:    apiV2Endps,
	}

	if opts.IsAutoRefresh {
		logger.Debugf("Auto-refresh; adding header Snap-Refresh-Reason: scheduled")
		reqOptions.addHeader("Snap-Refresh-Reason", "scheduled")
	}

	if useDeltas() {
		logger.Debugf("Deltas enabled. Adding header Snap-Accept-Delta-Format: %v", s.deltaFormat)
		reqOptions.addHeader("Snap-Accept-Delta-Format", s.deltaFormat)
	}
	if opts.RefreshManaged {
		reqOptions.addHeader("Snap-Refresh-Managed", "true")
	}

	var results snapActionResultList
	resp, err := s.retryRequestDecodeJSON(ctx, reqOptions, user, &results, nil)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != 200 {
		return nil, respToError(resp, "query the store for updates")
	}

	s.extractSuggestedCurrency(resp)

	refreshErrors := make(map[string]error)
	installErrors := make(map[string]error)
	downloadErrors := make(map[string]error)
	var otherErrors []error

	var sars []SnapActionResult
	for _, res := range results.Results {
		if res.Result == "error" {
			if a := installs[res.InstanceKey]; a != nil {
				if res.Name != "" {
					installErrors[a.InstanceName] = translateSnapActionError("install", a.Channel, res.Error.Code, res.Error.Message, res.Error.Extra.Releases)
					continue
				}
			} else if a := downloads[res.InstanceKey]; a != nil {
				if res.Name != "" {
					downloadErrors[res.Name] = translateSnapActionError("download", a.Channel, res.Error.Code, res.Error.Message, res.Error.Extra.Releases)
					continue
				}
			} else {
				if cur := curSnaps[res.InstanceKey]; cur != nil {
					a := refreshes[res.InstanceKey]
					if a == nil {
						// got an error for a snap that was not part of an 'action'
						otherErrors = append(otherErrors, translateSnapActionError("", "", res.Error.Code, fmt.Sprintf("snap %q: %s", cur.InstanceName, res.Error.Message), nil))
						logger.Debugf("Unexpected error for snap %q, instance key %v: [%v] %v", cur.InstanceName, res.InstanceKey, res.Error.Code, res.Error.Message)
						continue
					}
					channel := a.Channel
					if channel == "" && a.Revision.Unset() {
						channel = cur.TrackingChannel
					}
					refreshErrors[cur.InstanceName] = translateSnapActionError("refresh", channel, res.Error.Code, res.Error.Message, res.Error.Extra.Releases)
					continue
				}
			}
			otherErrors = append(otherErrors, translateSnapActionError("", "", res.Error.Code, res.Error.Message, nil))
			continue
		}
		snapInfo, err := infoFromStoreSnap(&res.Snap)
		if err != nil {
			return nil, fmt.Errorf("unexpected invalid install/refresh API result: %v", err)
		}

		snapInfo.Channel = res.EffectiveChannel

		var instanceName string
		if res.Result == "refresh" {
			cur := curSnaps[res.InstanceKey]
			if cur == nil {
				return nil, fmt.Errorf("unexpected invalid install/refresh API result: unexpected refresh")
			}
			rrev := snap.R(res.Snap.Revision)
			if rrev == cur.Revision || findRev(rrev, cur.Block) {
				refreshErrors[cur.InstanceName] = ErrNoUpdateAvailable
				continue
			}
			instanceName = cur.InstanceName
		} else if res.Result == "install" {
			if action := installs[res.InstanceKey]; action != nil {
				instanceName = action.InstanceName
			}
		}

		if res.Result != "download" && instanceName == "" {
			return nil, fmt.Errorf("unexpected invalid install/refresh API result: unexpected instance-key %q", res.InstanceKey)
		}

		_, instanceKey := snap.SplitInstanceName(instanceName)
		snapInfo.InstanceKey = instanceKey

		sars = append(sars, SnapActionResult{Info: snapInfo, RedirectChannel: res.RedirectChannel})
	}

	for _, errObj := range results.ErrorList {
		otherErrors = append(otherErrors, translateSnapActionError("", "", errObj.Code, errObj.Message, nil))
	}

	if len(refreshErrors)+len(installErrors)+len(downloadErrors) != 0 || len(results.Results) == 0 || len(otherErrors) != 0 {
		// normalize empty maps
		if len(refreshErrors) == 0 {
			refreshErrors = nil
		}
		if len(installErrors) == 0 {
			installErrors = nil
		}
		if len(downloadErrors) == 0 {
			downloadErrors = nil
		}
		return sars, &SnapActionError{
			NoResults: len(results.Results) == 0,
			Refresh:   refreshErrors,
			Install:   installErrors,
			Download:  downloadErrors,
			Other:     otherErrors,
		}
	}

	return sars, nil
}

func findRev(needle snap.Revision, haystack []snap.Revision) bool {
	for _, r := range haystack {
		if needle == r {
			return true
		}
	}
	return false
}