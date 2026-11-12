package oracle_test

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/pingqiu/sw-test-runner/packs/kv/oracle"
	"os"
	"sync"
	"testing"
)

func content(s string) oracle.Content {
	return oracle.Content{Length: uint64(len(s)), SHA256: sha256.Sum256([]byte(s))}
}

func TestRejectedACKKeepsTheWritePending(t *testing.T) {
	for _, kind := range []string{"sequence", "length", "fids", "fid-shape"} {
		t.Run(kind, func(t *testing.T) {
			o := oracle.New()
			v, e := o.BeginWrite("key", content("new bytes"))
			if e != nil {
				t.Fatal(e)
			}
			seq, n, fids := v.Sequence, uint64(9), []string{"1,abc"}
			switch kind {
			case "sequence":
				seq++
			case "length":
				n--
			case "fids":
				fids = nil
			case "fid-shape":
				fids = []string{"not-a-fid"}
			}
			if _, e = o.Acknowledge("key", seq, n, fids); e == nil {
				t.Fatal("foreign ACK accepted")
			}
			if _, e = o.BeginRead("key"); e == nil {
				t.Fatal("failed ACK exposed data")
			}
			if _, e = o.Acknowledge("key", v.Sequence, 9, []string{"1,abc"}); e != nil {
				t.Fatal("failed ACK consumed intent", e)
			}
		})
	}
}

func TestCallerCannotMutateACKOrReadVersion(t *testing.T) {
	o := oracle.New()
	v, _ := o.BeginWrite("key", content("old bytes"))
	fids := []string{"1,abc"}
	ack, e := o.Acknowledge("key", v.Sequence, 9, fids)
	if e != nil {
		t.Fatal(e)
	}
	fids[0] = "2,changed"
	ack.FIDs[0] = "3,changed"
	read, e := o.BeginRead("key")
	if e != nil {
		t.Fatal(e)
	}
	if read.FIDs[0] != "1,abc" {
		t.Fatal(read)
	}
	read.FIDs[0] = "4,changed"
	if _, e = o.FinishRead("key", 200, content("old bytes")); e != nil {
		t.Fatal(e)
	}
	if e = o.Retire("1", oracle.RetirementEvidence{true, true, true}); e != nil {
		t.Fatal(e)
	}
	if _, e = o.BeginRead("key"); e != nil {
		t.Fatal(e)
	}
	if got, e := o.FinishRead("key", 404, oracle.Content{}); e != nil || got != "retired_miss" {
		t.Fatal(got, e)
	}
}

func TestNonSuccessAndMissingGuardDoNotBecomeHits(t *testing.T) {
	o := oracle.New()
	if _, e := o.BeginRead("unacknowledged"); e == nil {
		t.Fatal("unacknowledged key exposed")
	}
	put(t, o, "key", "old bytes", "1,abc")
	if _, e := o.FinishRead("key", 200, content("old bytes")); e == nil {
		t.Fatal("unguarded read accepted")
	}
	for _, status := range []int{500, 503, 408} {
		if _, e := o.BeginRead("key"); e != nil {
			t.Fatal(e)
		}
		if _, e := o.FinishRead("key", status, content("old bytes")); e == nil {
			t.Fatal("unattributed error accepted", status)
		}
	}
}

func TestRetirementDoesNotExcuseWrongBytes(t *testing.T) {
	o := oracle.New()
	put(t, o, "key", "old bytes", "1,abc")
	if e := o.Retire("1", oracle.RetirementEvidence{true, true, true}); e != nil {
		t.Fatal(e)
	}
	if _, e := o.BeginRead("key"); e != nil {
		t.Fatal(e)
	}
	if _, e := o.FinishRead("key", 200, content("bad bytes")); e == nil {
		t.Fatal("retired volume excused corruption")
	}
}

func TestReadLengthIsCheckedEvenWhenDigestMatches(t *testing.T) {
	o := oracle.New()
	put(t, o, "key", "old bytes", "1,abc")
	if _, e := o.BeginRead("key"); e != nil {
		t.Fatal(e)
	}
	actual := content("old bytes")
	actual.Length--
	if _, e := o.FinishRead("key", 200, actual); e == nil {
		t.Fatal("short read receipt accepted")
	}
}

func TestIndependentKeysMayRunConcurrently(t *testing.T) {
	o := oracle.New()
	var group sync.WaitGroup
	for i := 0; i < 8; i++ {
		group.Add(1)
		go func(i int) {
			defer group.Done()
			key := fmt.Sprint(i)
			v, e := o.BeginWrite(key, content(key))
			if e != nil {
				t.Error(e)
				return
			}
			if _, e = o.Acknowledge(key, v.Sequence, uint64(len(key)), []string{"1,abc"}); e != nil {
				t.Error(e)
				return
			}
			if _, e = o.BeginRead(key); e != nil {
				t.Error(e)
				return
			}
			if _, e = o.FinishRead(key, 200, content(key)); e != nil {
				t.Error(e)
			}
		}(i)
	}
	group.Wait()
}

func digest(t *testing.T, s string) [32]byte {
	t.Helper()
	var d [32]byte
	b, e := hex.DecodeString(s)
	if e != nil || len(b) != 32 {
		t.Fatal("bad fixture digest", e)
	}
	copy(d[:], b)
	return d
}

// This is a receipt replay, not execution of a worker or qualification of its path.
func replay(t *testing.T, fixture string, corrupt bool) error {
	t.Helper()
	f, e := os.Open("testdata/" + fixture + "-receipts.jsonl")
	if e != nil {
		t.Fatal(e)
	}
	defer f.Close()
	o := oracle.New()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	versions := map[string]oracle.Version{}
	metadata := map[string]struct {
		Size uint64
		FIDs []string
	}{}
	reads := 0
	for scanner.Scan() {
		var envelope struct {
			Source string
			Record json.RawMessage
		}
		if e = json.Unmarshal(scanner.Bytes(), &envelope); e != nil {
			t.Fatal(e)
		}
		if envelope.Source == "ack-journal.jsonl" {
			var a struct {
				Key              string
				Sequence, Length uint64
				Digest           string
				FIDs             []string
			}
			if e = json.Unmarshal(envelope.Record, &a); e != nil {
				t.Fatal(e)
			}
			m := metadata[a.Key]
			v, e := o.Acknowledge(a.Key, a.Sequence, m.Size, m.FIDs)
			if e != nil {
				return e
			}
			if v.Content != (oracle.Content{Length: a.Length, SHA256: digest(t, a.Digest)}) {
				return fmt.Errorf("ACK journal differs from independent plan")
			}
			if fmt.Sprint(v.FIDs) != fmt.Sprint(a.FIDs) {
				return fmt.Errorf("metadata/journal FIDs differ")
			}
			continue
		}
		var r struct {
			Event   string
			Command struct {
				Action, Key, Seed string
				Length, Sequence  uint64
			}
			Row struct {
				Action, Key string
				Length      uint64
				SHA256      string
				Metadata    struct {
					FileSize uint64
					Chunks   []struct {
						FileID string `json:"file_id"`
					}
				}
			}
		}
		if e = json.Unmarshal(envelope.Record, &r); e != nil {
			t.Fatal(e)
		}
		if r.Event == "dispatched" {
			c := r.Command
			if c.Action == "set" {
				block := sha256.Sum256([]byte(c.Seed))
				payload := bytes.Repeat(block[:], (int(c.Length)+31)/32)[:int(c.Length)]
				v, e := o.BeginWrite(c.Key, oracle.Content{Length: c.Length, SHA256: sha256.Sum256(payload)})
				if e != nil {
					return e
				}
				if v.Sequence != c.Sequence {
					return fmt.Errorf("sequence differs from dispatch")
				}
				versions[c.Key] = v
			} else {
				if _, e = o.BeginRead(c.Key); e != nil {
					return e
				}
			}
		} else if r.Event == "complete" {
			c := r.Row
			if c.Action == "set" {
				var ids []string
				for _, chunk := range c.Metadata.Chunks {
					ids = append(ids, chunk.FileID)
				}
				metadata[c.Key] = struct {
					Size uint64
					FIDs []string
				}{c.Metadata.FileSize, ids}
			} else {
				actual := oracle.Content{Length: c.Length, SHA256: digest(t, c.SHA256)}
				if corrupt {
					actual.SHA256[0] ^= 1
				}
				if _, e = o.FinishRead(c.Key, 200, actual); e != nil {
					return e
				}
				reads++
			}
		}
	}
	if e = scanner.Err(); e != nil {
		return e
	}
	if len(versions) != 2 || reads != 2 {
		return fmt.Errorf("incomplete fixture replay")
	}
	return nil
}
func TestRetainedRealACKAndReadSubset(t *testing.T) {
	if e := replay(t, "v3a", false); e != nil {
		t.Fatal(e)
	}
}
func TestRetainedReadCorruptionRefused(t *testing.T) {
	if e := replay(t, "v3a", true); e == nil {
		t.Fatal("changed native digest accepted")
	}
}
func TestLatestACKRead(t *testing.T) {
	o := oracle.New()
	v, err := o.BeginWrite("key", content("old bytes"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = o.Acknowledge("key", v.Sequence, 9, []string{"1,abc"}); err != nil {
		t.Fatal(err)
	}
	if _, err = o.BeginRead("key"); err != nil {
		t.Fatal(err)
	}
	if hit, err := o.FinishRead("key", 200, content("old bytes")); err != nil || hit != "hit" {
		t.Fatal(hit, err)
	}
}

func put(t *testing.T, o *oracle.Oracle, key, data, fid string) oracle.Version {
	t.Helper()
	v, e := o.BeginWrite(key, content(data))
	if e != nil {
		t.Fatal(e)
	}
	v, e = o.Acknowledge(key, v.Sequence, uint64(len(data)), []string{fid})
	if e != nil {
		t.Fatal(e)
	}
	return v
}
func TestSameKeyGuardsSpanTheOperation(t *testing.T) {
	o := oracle.New()
	put(t, o, "key", "old bytes", "1,abc")
	if _, e := o.BeginRead("key"); e != nil {
		t.Fatal(e)
	}
	if _, e := o.BeginRead("key"); e == nil {
		t.Fatal("second reader admitted")
	}
	if _, e := o.BeginWrite("key", content("new bytes")); e == nil {
		t.Fatal("write overlaps reader")
	}
	put(t, o, "other", "parallel", "2,def")
	if _, e := o.FinishRead("key", 200, content("old bytes")); e != nil {
		t.Fatal(e)
	}
	v, e := o.BeginWrite("key", content("new bytes"))
	if e != nil {
		t.Fatal(e)
	}
	if _, e := o.BeginWrite("key", content("later")); e == nil {
		t.Fatal("write overlaps write")
	}
	if _, e := o.BeginRead("key"); e == nil {
		t.Fatal("read overlaps write")
	}
	if _, e := o.Acknowledge("key", v.Sequence, 9, []string{"3,xyz"}); e != nil {
		t.Fatal(e)
	}
	if _, e := o.BeginRead("key"); e != nil {
		t.Fatal(e)
	}
	if _, e := o.FinishRead("key", 200, content("old bytes")); e == nil {
		t.Fatal("stale same-length bytes accepted")
	}
}

func TestCommittedResponseLostNeverPromotesLateACK(t *testing.T) {
	o := oracle.New()
	put(t, o, "key", "old bytes", "1,abc")
	v, e := o.BeginWrite("key", content("new bytes"))
	if e != nil {
		t.Fatal(e)
	}
	// A fake volume may have committed the new bytes; the caller lost the reply.
	if e = o.UncertainWrite("key"); e == nil {
		t.Fatal("unknown outcome reported success")
	}
	if _, e = o.Acknowledge("key", v.Sequence, 9, []string{"2,def"}); e == nil {
		t.Fatal("late success promoted UNKNOWN to ACK")
	}
	if _, e = o.BeginRead("key"); e == nil {
		t.Fatal("unknown read exposed old version")
	}
	if _, e = o.BeginWrite("key", content("retry")); e == nil {
		t.Fatal("unknown mutation replayed")
	}
}

func TestMissRequiresAllCurrentVersionVolumesRetired(t *testing.T) {
	o := oracle.New()
	v, e := o.BeginWrite("key", content("old bytes"))
	if e != nil {
		t.Fatal(e)
	}
	if _, e = o.Acknowledge("key", v.Sequence, 9, []string{"1,abc", "2,def"}); e != nil {
		t.Fatal(e)
	}
	for _, proof := range []oracle.RetirementEvidence{{false, true, true}, {true, false, true}, {true, true, false}} {
		if e = o.Retire("1", proof); e == nil {
			t.Fatal("unproved retirement accepted")
		}
	}
	miss := func(want string) {
		t.Helper()
		if _, e := o.BeginRead("key"); e != nil {
			t.Fatal(e)
		}
		got, e := o.FinishRead("key", 404, oracle.Content{})
		if want == "" {
			if e == nil {
				t.Fatal("unexpected miss accepted")
			}
		} else if e != nil || got != want {
			t.Fatal(got, e)
		}
	}
	miss("")
	proof := oracle.RetirementEvidence{true, true, true}
	if e = o.Retire("1", proof); e != nil {
		t.Fatal(e)
	}
	miss("")
	if e = o.Retire("2", proof); e != nil {
		t.Fatal(e)
	}
	miss("retired_miss")
	put(t, o, "key", "new bytes", "3,xyz")
	miss("")
}

func TestAcceptedV2ReceiptSubset(t *testing.T) {
	if e := replay(t, "v2", false); e != nil {
		t.Fatal(e)
	}
}

func TestRetainedFixtureByteIdentity(t *testing.T) {
	for _, pair := range [][2]string{{"provenance.json", "v3a-receipts.jsonl"}, {"v2-provenance.json", "v2-receipts.jsonl"}} {
		raw, e := os.ReadFile("testdata/" + pair[0])
		if e != nil {
			t.Fatal(e)
		}
		var provenance struct {
			FixtureSHA256 string `json:"fixture_sha256"`
		}
		if e = json.Unmarshal(raw, &provenance); e != nil {
			t.Fatal(e)
		}
		raw, e = os.ReadFile("testdata/" + pair[1])
		if e != nil {
			t.Fatal(e)
		}
		if sha256.Sum256(raw) != digest(t, provenance.FixtureSHA256) {
			t.Fatal("retained fixture bytes changed", pair[1])
		}
	}
}
