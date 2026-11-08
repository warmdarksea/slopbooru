package main

import (
	"flag"
	"fmt"
	"os"
	"sort"

	"imgrec/internal/records"
)

// imgrec rebuild-store: the store (what the booru reads) is derived from
// the records, so it can always be rebuilt from them -- after a parser
// fix, or to drop rows a past bug left behind. Records answering the
// current specs are replayed, oldest first; answers to superseded
// questions stay in the records but leave the store. Edits by hand
// (provenance "manual") are kept.
func rebuildCmd(args []string) error {
	fs := flag.NewFlagSet("rebuild-store", flag.ExitOnError)
	var f fleet
	f.flags(fs)
	fs.Parse(args)
	p, _, closeAll, err := f.open()
	if err != nil {
		return err
	}
	defer closeAll()
	current := map[string]string{}
	for name, s := range p.Specs {
		current[name] = s.Hash()
	}
	var recs []records.Record
	if err := p.Records.Scan(func(r records.Record) error {
		if current[r.Model] == r.SpecHash {
			recs = append(recs, r)
		}
		return nil
	}); err != nil {
		return err
	}
	sort.SliceStable(recs, func(i, j int) bool { return recs[i].At < recs[j].At })
	db := p.Store.DB
	if _, err := db.Exec("DELETE FROM annotations WHERE provenance_id NOT IN (SELECT id FROM provenances WHERE model = 'manual')"); err != nil {
		return err
	}
	if _, err := db.Exec("DELETE FROM failures"); err != nil {
		return err
	}
	written := 0
	for i := range recs {
		if err := p.Materialize(recs[i].Model, recs[i].ImageSHA, &recs[i]); err != nil {
			fmt.Fprintf(os.Stderr, "imgrec: %s %s: %v\n", recs[i].Model, recs[i].ImageSHA[:12], err)
			continue
		}
		written++
	}
	fmt.Fprintf(os.Stderr, "imgrec: store rebuilt from %d record(s) of the current specs\n", written)
	return nil
}
