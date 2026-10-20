package index

import (
	"fmt"
	"testing"
)

const tsSample = `import { Router } from "express";

/* A block comment with a { brace
   that spans lines } */
export interface BlobRecord {
  key: string;
  size: number;
}

export type BlobKey = string;
type Handler<T> = (input: T) => void;

export enum Visibility { Public, Private }
export const enum Mode { Read, Write }

export const MAX_BLOB_BYTES = 5 * 1024 * 1024;
export const makeKey = (name: string): BlobKey => ` + "`blobs/${name}`" + `;
const helper = async function () { return 1; };
const { a, b } = config;

export default async function uploadBlob(file: File) {
  if (file.size > MAX_BLOB_BYTES) {
    throw new Error("too big { not a brace");
  }
}

export abstract class BlobStore<T> implements Store {
  private readonly cache = new Map<string, T>();
  #secret = 1;

  constructor(private root: string) {
    super();
  }

  async put(key: string, data: Buffer): Promise<void> {
    if (!data) {
      return;
    }
    const inner = () => { return 1; };
  }

  static fromEnv(): BlobStore<unknown> {
    return null as any;
  }

  get size(): number { return this.cache.size; }

  remove = async (key: string) => {
    this.cache.delete(key);
  };

  private template = ` + "`" + `
    multi-line { template
  ` + "`" + `;

  #hidden() {}
}

class Local extends BlobStore<string> {
  read(key: string) {}
}

export default class extends Controller {
  connect() {}
}
`

func TestParseTypeScript_FindsDeclarationsAndMethods(t *testing.T) {
	syms := ParseTypeScript("src/server/blobs.ts", []byte(tsSample))

	got := map[string]bool{}
	for _, s := range syms {
		got[fmt.Sprintf("%s %s %s %d", s.Kind, s.Container, s.Name, s.Line)] = true
	}

	for _, want := range []string{
		"interface  BlobRecord 5",
		"type  BlobKey 10",
		"type  Handler 11",
		"enum  Visibility 13",
		"enum  Mode 14",
		"const  MAX_BLOB_BYTES 16",
		"function  makeKey 17",
		"function  helper 18",
		"function  uploadBlob 21",
		"class  BlobStore 27",
		"method BlobStore constructor 31",
		"method BlobStore put 35",
		"method BlobStore fromEnv 42",
		"method BlobStore size 46",
		"method BlobStore remove 48",
		"method BlobStore #hidden 56",
		"class  Local 59",
		"method Local read 60",
		"method  connect 64",
	} {
		if !got[want] {
			t.Errorf("missing %q", want)
		}
	}

	for _, s := range syms {
		switch {
		case s.Name == "a" || s.Name == "b":
			t.Errorf("a destructuring assignment was indexed: %+v", s)
		case s.Name == "if" || s.Name == "inner" || s.Name == "cache" || s.Name == "template":
			t.Errorf("a statement, a local or a plain field was indexed as a method: %+v", s)
		case s.Container == "BlobStore" && s.Line > 58:
			t.Errorf("BlobStore did not close, so %q was filed under it", s.Name)
		}
	}
}

func TestShouldIndex_CoversScriptsButNotDeclarationFiles(t *testing.T) {
	for path, want := range map[string]bool{
		"app/models/user.rb":       true,
		"src/server/blobs.ts":      true,
		"src/client/ui/panel.tsx":  true,
		"app/javascript/editor.js": true,
		"src/types/vendor.d.ts":    false,
		"db/migrate/20260101_x.rb": false,
		"README.md":                false,
	} {
		if got := shouldIndex(path); got != want {
			t.Errorf("shouldIndex(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestMappable_NamesThatCanStandForAFeatureMember(t *testing.T) {
	for _, tc := range []struct {
		sym  Symbol
		want bool
	}{
		{Symbol{Name: "BlobStore", Kind: "class"}, true},
		{Symbol{Name: "BlobRecord", Kind: "interface"}, true},
		{Symbol{Name: "uploadBlob", Kind: "function"}, true},
		{Symbol{Name: "MAX_BLOB_BYTES", Kind: "const"}, true},
		{Symbol{Name: "put", Kind: "method", Container: "BlobStore"}, false},
		{Symbol{Name: "TIMEOUT", Kind: "constant", Container: "Client"}, false},
	} {
		if got := Mappable(tc.sym); got != tc.want {
			t.Errorf("Mappable(%+v) = %v, want %v", tc.sym, got, tc.want)
		}
	}
}
