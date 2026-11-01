package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const (
	sliceProducts = "External ID,Name,Price\r\n"
	sliceContacts = "First name,Contact key\r\n"
	sliceDeals    = "ID,Deal key,Deal name,Contact,Contact key,Product\r\n"
)

func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	os.MkdirAll(dir, 0o755)
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// 12 deals: 8 buyers with two products each, a second deal of buyer C8, a one-product buyer, two with Contact empty.
func sliceFixture() map[string]string {
	var products, contacts, deals strings.Builder
	products.WriteString(sliceProducts)
	contacts.WriteString(sliceContacts)
	deals.WriteString(sliceDeals)
	id := 0
	deal := func(buyer, name string, items ...string) {
		id++
		for _, item := range items {
			fmt.Fprintf(&deals, "%d,D%d,Sale %d,%s,%s,%s\r\n", id, id, id, name, buyer, item)
		}
	}
	for i := 1; i <= 8; i++ {
		fmt.Fprintf(&contacts, "N%d,C%d\r\n", i, i)
		fmt.Fprintf(&products, "A%d,Item%d,1\r\nB%d,Other%d,1\r\n", i, i, i, i)
		deal(fmt.Sprintf("C%d", i), fmt.Sprintf("N%d", i), fmt.Sprintf("Item%d", i), fmt.Sprintf("Other%d", i))
	}
	deal("C8", "N8", "Item8")
	contacts.WriteString("Lone,C9\r\nShared,C10\r\nShared,C11\r\nNever bought,C12\r\n")
	products.WriteString("Z,Unsold,1\r\n")
	deal("C9", "Lone", "Item1")
	deal("C10", "", "Item1")
	deal("C11", "", "Item2")
	return map[string]string{"products.csv": products.String(), "offline_contact.csv": contacts.String(), "offline_deals.csv": deals.String()}
}

func TestSliceLinksUpAndLeavesProductionAlone(t *testing.T) {
	dir := t.TempDir()
	prod, test := filepath.Join(dir, "production"), filepath.Join(dir, "test")
	writeFiles(t, prod, sliceFixture())
	if made, err := writeTestSlice(prod, test); err != nil || !made {
		t.Fatal(made, err)
	}

	for name, want := range sliceFixture() {
		if read(t, filepath.Join(prod, name)) != want {
			t.Fatalf("%s in production changed", name)
		}
	}
	deals, _ := readCSV(filepath.Join(test, "offline_deals.csv"))
	contacts, _ := readCSV(filepath.Join(test, "offline_contact.csv"))
	products, _ := readCSV(filepath.Join(test, "products.csv"))
	if !deals.crlf || !contacts.crlf || !products.crlf {
		t.Fatal("the test files must keep the line endings of production")
	}

	recs := records(deals.rows, true)
	if len(recs) != 10 {
		t.Fatalf("want 10 deals, got %d: %v", len(recs), deals.rows)
	}
	buyers, sold := map[string]int{}, map[string]bool{}
	for i, rec := range recs {
		if rec[0][0] != fmt.Sprint(i+1) {
			t.Fatalf("deal %d must be numbered from 1: %v", i, rec)
		}
		if !strings.HasPrefix(rec[0][2], "TEST Sale ") {
			t.Fatalf("a test deal must be findable by its name: %v", rec[0])
		}
		buyers[rec[0][4]]++
		for _, row := range rec {
			sold[row[5]] = true
		}
	}
	if buyers["C8"] != 2 {
		t.Fatalf("one buyer must have two deals: %v", buyers)
	}
	if buyers["C10"] != 1 || buyers["C11"] != 1 {
		t.Fatalf("both deals with an empty Contact must be there: %v", buyers)
	}
	for _, c := range contacts.rows {
		delete(buyers, c[1])
	}
	if len(buyers) != 0 {
		t.Fatalf("every buyer needs a contact row, missing: %v", buyers)
	}
	for _, c := range contacts.rows {
		if c[1] == "C12" {
			t.Fatal("a contact without a test deal must not be in the test file")
		}
	}
	for _, p := range products.rows {
		if !sold[p[1]] {
			t.Fatalf("product %v is in no test deal", p)
		}
		delete(sold, p[1])
	}
	if len(sold) != 0 {
		t.Fatalf("products sold in test deals but not in the test file: %v", sold)
	}
}

func TestSliceLeftAloneWhenNothingUsable(t *testing.T) {
	dir := t.TempDir()
	prod, test := filepath.Join(dir, "production"), filepath.Join(dir, "test")
	writeFiles(t, test, map[string]string{"products.csv": "kept"})

	// a file missing
	writeFiles(t, prod, map[string]string{"products.csv": sliceProducts + "A,Item,1\r\n"})
	if made, err := writeTestSlice(prod, test); err != nil || made {
		t.Fatal(made, err)
	}
	// a small update: one deal's buyer is not in it, another's product is not, a third would link but is too few
	writeFiles(t, prod, map[string]string{
		"offline_contact.csv": sliceContacts + "N,C1\r\n",
		"offline_deals.csv":   sliceDeals + "1,D1,S,X,C99,Item\r\n2,D2,S,N,C1,Unknown\r\n3,D3,S,N,C1,Item\r\n4,D4,S,,,Item\r\n",
	})
	if made, err := writeTestSlice(prod, test); err != nil || made {
		t.Fatal(made, err)
	}
	if read(t, filepath.Join(test, "products.csv")) != "kept" {
		t.Fatal("the test folder must be left as it was")
	}
}

func TestDownloadMakesProductionAndTestWithTheAgreedNames(t *testing.T) {
	fx := sliceFixture()
	f := &fake{ids: []string{"2026-10-14_030000"}, files: map[string]map[string]string{"2026-10-14_030000": {
		"products.csv": fx["products.csv"], "contacts.csv": fx["offline_contact.csv"], "deals.csv": fx["offline_deals.csv"],
		"IMPORT_ORDER.txt": "1. products.csv\r\n",
	}}}
	cfg := testConfig(t, newFake(t, f).URL)
	res, err := run(cfg, noPull, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(res.Files, []fileRows{{"products.csv", 17}, {"offline_contact.csv", 12}, {"offline_deals.csv", 12}}) {
		t.Fatalf("%+v", res.Files)
	}
	want := []string{"offline_contact.csv", "offline_deals.csv", "products.csv"}
	for _, dir := range []string{cfg.production(), cfg.test()} {
		var got []string
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			got = append(got, e.Name())
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s holds %v", dir, got)
		}
	}
	if !res.TestReady {
		t.Fatal("the test folder was just made")
	}
	entries, _ := os.ReadDir(cfg.Folder)
	if len(entries) != 2 {
		t.Fatalf("the main folder must hold only production and test, got %d", len(entries))
	}
	msg := summary(cfg, res)
	for _, s := range []string{"products.csv, then offline_contact.csv, then offline_deals.csv", cfg.test(), "delete the test deals"} {
		if !strings.Contains(msg, s) {
			t.Fatalf("message lacks %q:\n%s", s, msg)
		}
	}
}

func TestFailedTestSliceHarmsNothing(t *testing.T) {
	fx := sliceFixture()
	f := &fake{ids: []string{"2026-10-14_030000"}, files: map[string]map[string]string{"2026-10-14_030000": {
		"products.csv": fx["products.csv"], "contacts.csv": fx["offline_contact.csv"],
		"deals.csv": "ID,Deal key\r\n1,D1\r\n", // no Contact key or Product: the slice cannot be made
	}}}
	cfg := testConfig(t, newFake(t, f).URL)
	var out strings.Builder
	res, err := run(cfg, noPull, &out)
	if err != nil || res.TestReady || !strings.Contains(out.String(), "production files are fine") {
		t.Fatalf("%+v %v %q", res, err, out.String())
	}
	if read(t, filepath.Join(cfg.production(), "products.csv")) != fx["products.csv"] || len(res.Files) != 3 {
		t.Fatalf("production must be intact: %+v", res.Files)
	}
	done, _ := readState(cfg.State)
	if !done["2026-10-14_030000"] {
		t.Fatal("the bundle must stay on the downloaded list")
	}
	if strings.Contains(summary(cfg, res), "small files") {
		t.Fatal("must not recommend test files that were not made")
	}
}

func TestEmptiedTestFolderIsRemade(t *testing.T) {
	fx := sliceFixture()
	f := &fake{ids: []string{"2026-10-14_030000"}, files: map[string]map[string]string{"2026-10-14_030000": {
		"products.csv": fx["products.csv"], "contacts.csv": fx["offline_contact.csv"], "deals.csv": fx["offline_deals.csv"],
	}}}
	cfg := testConfig(t, newFake(t, f).URL)
	if _, err := run(cfg, noPull, io.Discard); err != nil {
		t.Fatal(err)
	}
	os.Remove(filepath.Join(cfg.test(), "products.csv"))
	res, err := run(cfg, noPull, io.Discard)
	if err != nil || !res.TestReady {
		t.Fatalf("%+v %v", res, err)
	}
	if entries, _ := os.ReadDir(cfg.test()); len(entries) != 3 {
		t.Fatalf("got %d files", len(entries))
	}
}

func TestSwapKeepsOldFolderWhenTheNewOneCannotMove(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "test")
	writeFiles(t, dst, map[string]string{"a.csv": "old"})
	if err := swapFolder(filepath.Join(dir, "missing"), dst); err == nil {
		t.Fatal("want an error")
	}
	if read(t, filepath.Join(dst, "a.csv")) != "old" {
		t.Fatal("the old folder must be back")
	}
}
