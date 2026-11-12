package seaweed

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
)

func maintenanceCollection(scopes map[string]maintenanceScope) (map[string][]string, error) {
	if len(scopes) != 6 {
		return nil, errors.New("collection owner observation incomplete")
	}
	owners := map[string][]string{}
	for id, s := range scopes {
		if s.Pending != 0 {
			return nil, errors.New("collection has unresolved files or switches")
		}
		for vid, readonly := range s.Volumes {
			n, err := strconv.ParseUint(vid, 10, 32)
			if err != nil || n == 0 || strconv.FormatUint(n, 10) != vid || readonly {
				return nil, errors.New("collection VID/readiness is not proved")
			}
			owners[vid] = append(owners[vid], id)
		}
	}
	for vid := range owners {
		sort.Strings(owners[vid])
	}
	return owners, nil
}

func maintenanceFreezeCollection(scopes map[string]maintenanceScope, primary, task string) (map[string]bool, error) {
	owners, err := maintenanceCollection(scopes)
	if err != nil {
		return nil, err
	}
	if len(owners[primary]) != 2 || task == "volume.balance" && len(owners) < 5 {
		return nil, errors.New("maintenance fixture lacks real replicated VID imbalance")
	}
	frozen := map[string]bool{}
	for vid, ids := range owners {
		if len(ids) != 2 {
			return nil, errors.New("fixture lacks two bound owners per VID")
		}
		frozen[vid] = true
	}
	return frozen, nil
}

func (m *maintenanceRuntime) verifyCollection(ctx context.Context, scopes map[string]maintenanceScope, unchangedRefusal bool) error {
	owners, err := maintenanceCollection(scopes)
	if err != nil {
		return err
	}
	if len(owners) != len(m.fixtureVIDs) {
		return errors.New("post-task collection VID set changed")
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	var failure error
	prior := map[string][]string{}
	if unchangedRefusal {
		prior, err = maintenanceCollection(m.beforeTask)
		if err != nil {
			return err
		}
	}
	for vid, ids := range owners {
		wantCount := 2
		if unchangedRefusal {
			wantCount = len(prior[vid])
		}
		if !m.fixtureVIDs[vid] || wantCount < 1 || len(ids) != wantCount {
			return errors.New("post-task collection has missing/extra owners")
		}
	}
	for vid, ids := range owners {
		wg.Add(1)
		go func(vid string, ids []string) {
			defer wg.Done()
			out, e := m.r.step(ctx, "meta", "maintenance-lookup-"+vid, 15, "curl -fsS --max-time 10 "+cloudQuote("http://"+endpoint(m.hosts["meta"], "master_http")+"/dir/lookup?volumeId="+vid+"&collection="+m.manifest.Collection))
			var got []string
			if e == nil {
				got, e = maintenanceLookupURLs([]byte(out))
			}
			want := []string{}
			for _, id := range ids {
				want = append(want, endpoint(m.hosts[id], "volume_http"))
			}
			sort.Strings(want)
			sort.Strings(got)
			if e == nil && !reflect.DeepEqual(got, want) {
				e = fmt.Errorf("master lookup differs from bound owners for VID %s", vid)
			}
			mu.Lock()
			failure = errors.Join(failure, e)
			mu.Unlock()
		}(vid, ids)
	}
	wg.Wait()
	return failure
}

func maintenanceLookupURLs(raw []byte) ([]string, error) {
	if err := oneStrictValue(raw); err != nil {
		return nil, err
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		return nil, errors.New("lookup is not an object")
	}
	field := func(obj map[string]json.RawMessage, key string) (json.RawMessage, error) {
		for k := range obj {
			if k != key && strings.EqualFold(k, key) {
				return nil, fmt.Errorf("lookup aliases %s", key)
			}
		}
		return obj[key], nil
	}
	problem, err := field(object, "error")
	if err != nil {
		return nil, err
	}
	if problem != nil {
		var message string
		if bytes.Equal(bytes.TrimSpace(problem), []byte("null")) || json.Unmarshal(problem, &message) != nil || message != "" {
			return nil, errors.New("lookup reported an error")
		}
	}
	locations, err := field(object, "locations")
	if err != nil {
		return nil, err
	}
	var entries []map[string]json.RawMessage
	if locations == nil || bytes.Equal(bytes.TrimSpace(locations), []byte("null")) || json.Unmarshal(locations, &entries) != nil {
		return nil, errors.New("lookup has no typed locations")
	}
	urls := []string{}
	for _, entry := range entries {
		value, e := field(entry, "url")
		if e != nil {
			return nil, e
		}
		var url string
		if value == nil || json.Unmarshal(value, &url) != nil || url == "" {
			return nil, errors.New("lookup location has no URL")
		}
		urls = append(urls, url)
	}
	return urls, nil
}
