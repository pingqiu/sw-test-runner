package seaweed

import "testing"

func TestEC1aExactWithoutWitnessCannotEarnExposure(t *testing.T) {
	result, err := AssessEC1a(EC1aManifest{RunID: "run", Generation: "encode-1", Objects: []EC1aObject{{Key: "k", FID: "1,abc", Length: 1, SHA256: "ca978112ca1bbdcafac231b39a23dc4da786eff8147c4e72b9807785afee48bb", ACK: true}}}, []EC1aRead{{Op: chaosOp{Op: 0, Kind: "s3_get", Key: "k", Started: 1, Ended: 2, Exact: true}, Route: "s3", Status: 200, Length: 1, SHA256: "ca978112ca1bbdcafac231b39a23dc4da786eff8147c4e72b9807785afee48bb"}}, 30)
	if err != nil || result.Integrity != "PASS" || result.Exposure != "NOT_OBSERVED" {
		t.Fatalf("exact bytes may pass integrity, never inferred exposure: %+v %v", result, err)
	}
}

func TestEC1aMandatoryReadHasNoFaultExcusal(t *testing.T) {
	m := EC1aManifest{RunID: "run", Generation: "encode-1", Objects: []EC1aObject{{Key: "k", FID: "1,abc", Length: 1, SHA256: "ca978112ca1bbdcafac231b39a23dc4da786eff8147c4e72b9807785afee48bb", ACK: true}}}
	base := EC1aRead{Op: chaosOp{Op: 0, Kind: "s3_get", Key: "k", Started: 1, Ended: 2, Exact: true}, Route: "s3", Status: 200, Length: 1, SHA256: m.Objects[0].SHA256}
	for _, tc := range []struct {
		name   string
		change func(*EC1aRead)
	}{
		{"wrong byte", func(r *EC1aRead) { r.SHA256 = "0000000000000000000000000000000000000000000000000000000000000000" }},
		{"404", func(r *EC1aRead) { r.Status = 404 }},
		{"500", func(r *EC1aRead) { r.Status = 500 }},
		{"late exact", func(r *EC1aRead) { r.Op.Ended = 31.001 }},
		{"short", func(r *EC1aRead) { r.Length = 0 }},
		{"wrong key", func(r *EC1aRead) { r.Op.Key = "foreign" }},
		{"unobserved terminal", func(r *EC1aRead) { r.Op.Exact = false }},
		{"duplicate op", func(r *EC1aRead) { r.Op.Op = 1 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := base
			tc.change(&r)
			if _, e := AssessEC1a(m, []EC1aRead{r}, 30); e == nil {
				t.Fatal("invalid mandatory read accepted")
			}
		})
	}
	// Exact threshold and the second route are separate positives, never inferred from S3.
	base.Op.Ended = 31
	if _, e := AssessEC1a(m, []EC1aRead{base}, 30); e != nil {
		t.Fatal(e)
	}
	base.Route = "volume"
	if _, e := AssessEC1a(m, []EC1aRead{base}, 30); e != nil {
		t.Fatal(e)
	}
	m.Objects = append(m.Objects, EC1aObject{Key: "omitted", FID: "2,def", Length: 1, SHA256: base.SHA256, ACK: true})
	if _, e := AssessEC1a(m, []EC1aRead{base}, 30); e == nil {
		t.Fatal("omitted ACKed object accepted")
	}
}
