package tool

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

// bigScript is a TypeScript file past SkeletonThreshold, so it outlines.
func bigScript() string {
	var b strings.Builder
	b.WriteString(`import { Router } from "express";
import {
  readFile,
  writeFile,
} from "node:fs/promises";

export interface BlobRecord {
  key: string;
  size: number;
}

export type BlobKey = string;

export const MAX_BLOB_BYTES = 5 * 1024 * 1024;

export class BlobStore {
  constructor(private root: string) {}

  async put(key: BlobKey, data: Buffer): Promise<void> {
    const inner = { a: 1 };
    await writeFile(this.root + key, data);
  }

  get size(): number { return 0; }
}

export function makeKey(name: string): BlobKey {
  return "blobs/" + name;
}

export const shout = (s: string) => s.toUpperCase()

class Local extends BlobStore {
  put(key: BlobKey, data: Buffer): Promise<void> {
    return super.put(key, data);
  }
}
`)
	for i := 0; i < SkeletonThreshold; i++ {
		fmt.Fprintf(&b, "// filler %d\n", i)
	}
	return b.String()
}

func TestProcessScript_OutlinesALargeFile(t *testing.T) {
	var out bytes.Buffer
	if err := ProcessScript("src/server/blobs.ts", strings.NewReader(bigScript()), &out); err != nil {
		t.Fatal(err)
	}
	text := out.String()

	for _, want := range []string{
		"skeleton view",
		`import { Router } from "express";`,
		`import { readFile, writeFile, } from "node:fs/promises";`,
		"export interface BlobRecord {",
		"  size: number;",
		"export type BlobKey = string;",
		"export const MAX_BLOB_BYTES = 5 * 1024 * 1024;",
		"export class BlobStore {",
		"  async put(key: BlobKey, data: Buffer): Promise<void> { … }",
		"  get size(): number { return 0; }", // a one-line body is shorter whole
		"export function makeKey(name: string): BlobKey { … }",
		"export const shout = (s: string) => s.toUpperCase()",
		"class Local extends BlobStore {",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("outline is missing %q:\n%s", want, text)
		}
	}
	for _, unwanted := range []string{"await writeFile", "const inner", "filler"} {
		if strings.Contains(text, unwanted) {
			t.Errorf("outline leaked a body line %q:\n%s", unwanted, text)
		}
	}
	for _, close := range []string{"25\t}\n", "37\t}\n"} {
		if strings.Count(text, close) != 1 {
			t.Errorf("expected the class ending at %q closed once:\n%s", close, text)
		}
	}
}

func TestProcessScript_SmallFileComesBackWhole(t *testing.T) {
	var out bytes.Buffer
	src := "export const a = 1;\nexport function f() {\n  return a;\n}\n"
	if err := ProcessScript("src/a.ts", strings.NewReader(src), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out.String(), "# rts: 4 lines (full)") || !strings.Contains(out.String(), "return a;") {
		t.Errorf("a small file should come back in full:\n%s", out.String())
	}
}

func TestExtractScriptDefinitions_ByBareAndQualifiedName(t *testing.T) {
	got, err := ExtractScriptDefinitions("src/server/blobs.ts", strings.NewReader(bigScript()),
		[]string{"makeKey", "BlobStore.put", "put", "BlobRecord", "shout", "nope"})
	if err != nil {
		t.Fatal(err)
	}

	if want := "export function makeKey(name: string): BlobKey {\n  return \"blobs/\" + name;\n}"; got["makeKey"] != want {
		t.Errorf("makeKey = %q, want %q", got["makeKey"], want)
	}
	if !strings.Contains(got["BlobStore.put"], "await writeFile") || strings.Contains(got["BlobStore.put"], "super.put") {
		t.Errorf("BlobStore.put should be BlobStore's put alone:\n%s", got["BlobStore.put"])
	}
	if !strings.Contains(got["put"], "// BlobStore.put, line 19") || !strings.Contains(got["put"], "// Local.put, line 34") {
		t.Errorf("a bare name defined twice should return both, labelled:\n%s", got["put"])
	}
	if !strings.HasSuffix(got["BlobRecord"], "  size: number;\n}") {
		t.Errorf("an interface should come back whole:\n%s", got["BlobRecord"])
	}
	if got["shout"] != "export const shout = (s: string) => s.toUpperCase()" {
		t.Errorf("a one-line arrow without a semicolon should end on its line: %q", got["shout"])
	}
	if _, ok := got["nope"]; ok {
		t.Error("a name the file does not define should be absent")
	}
}

func TestListScriptDefinitions_OffersQualifiedNames(t *testing.T) {
	names, err := ListScriptDefinitions("src/server/blobs.ts", strings.NewReader(bigScript()))
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(names, ",")
	for _, want := range []string{"BlobStore", "put", "BlobStore.put", "Local.put", "makeKey"} {
		if !strings.Contains(","+joined+",", ","+want+",") {
			t.Errorf("missing %q in %v", want, names)
		}
	}
}
