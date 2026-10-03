package model

import (
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"reflect"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// The CRDs in testdata are manifests/crds/{application,appproject}-crd.yaml of the two
// supported Argo CD tags (v3.5.3 and v3.4.9). Every field this package declares for
// those kinds must exist in the schema of both versions, with a compatible JSON type.

func loadSchema(t *testing.T, file string) map[string]any {
	t.Helper()
	f, err := os.Open(file)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	var crd map[string]any
	if err := yaml.Unmarshal(b, &crd); err != nil {
		t.Fatal(err)
	}
	spec := crd["spec"].(map[string]any)
	for _, v := range spec["versions"].([]any) {
		vm := v.(map[string]any)
		if vm["name"] == "v1alpha1" {
			return vm["schema"].(map[string]any)["openAPIV3Schema"].(map[string]any)
		}
	}
	t.Fatalf("%s: no v1alpha1 version", file)
	return nil
}

func jsonName(f reflect.StructField) (string, bool) {
	tag := f.Tag.Get("json")
	if tag == "-" {
		return "", false
	}
	name := strings.Split(tag, ",")[0]
	if name == "" {
		name = f.Name
	}
	return name, true
}

// walk checks type typ against schema s and returns the problems found.
func walk(path string, typ reflect.Type, s map[string]any) []string {
	for typ.Kind() == reflect.Ptr {
		typ = typ.Elem()
	}
	var problems []string
	st, _ := s["type"].(string)
	switch typ.Kind() {
	case reflect.Struct:
		if typ.String() == "time.Time" {
			if st != "string" {
				problems = append(problems, fmt.Sprintf("%s: time wants string, schema %s", path, st))
			}
			return problems
		}
		props, _ := s["properties"].(map[string]any)
		if st != "object" || props == nil {
			if st == "object" && s["x-kubernetes-preserve-unknown-fields"] == true {
				return nil // free-form object (e.g. metadata)
			}
			return []string{fmt.Sprintf("%s: struct wants an object with properties, schema %q", path, st)}
		}
		for i := 0; i < typ.NumField(); i++ {
			f := typ.Field(i)
			name, ok := jsonName(f)
			if !ok {
				continue
			}
			sub, found := props[name].(map[string]any)
			if !found {
				problems = append(problems, fmt.Sprintf("%s.%s: not in the CRD schema", path, name))
				continue
			}
			problems = append(problems, walk(path+"."+name, f.Type, sub)...)
		}
	case reflect.Slice:
		if typ.Elem().Kind() == reflect.String && st == "array" {
			return nil
		}
		if st != "array" {
			return []string{fmt.Sprintf("%s: slice wants array, schema %q", path, st)}
		}
		items, _ := s["items"].(map[string]any)
		if typ.Elem().Kind() == reflect.Map {
			return nil
		}
		problems = append(problems, walk(path+"[]", typ.Elem(), items)...)
	case reflect.Map:
		if st != "object" {
			problems = append(problems, fmt.Sprintf("%s: map wants object, schema %q", path, st))
		}
	case reflect.String:
		if st != "string" {
			problems = append(problems, fmt.Sprintf("%s: string, schema %q", path, st))
		}
	case reflect.Bool:
		if st != "boolean" {
			problems = append(problems, fmt.Sprintf("%s: bool, schema %q", path, st))
		}
	case reflect.Int, reflect.Int64:
		if st != "integer" {
			problems = append(problems, fmt.Sprintf("%s: int, schema %q", path, st))
		}
	}
	return problems
}

func TestModelMatchesCRDs(t *testing.T) {
	for _, ver := range []string{"v3.5.3", "v3.4.9"} {
		t.Run(ver+"/Application", func(t *testing.T) {
			s := loadSchema(t, "testdata/application-crd-"+ver+".yaml.gz")
			props := s["properties"].(map[string]any)
			typ := reflect.TypeOf(Application{})
			for i := 0; i < typ.NumField(); i++ {
				f := typ.Field(i)
				name, _ := jsonName(f)
				if name == "metadata" {
					continue // ObjectMeta is not in the CRD schema
				}
				sub, ok := props[name].(map[string]any)
				if !ok {
					t.Errorf("Application.%s not in schema", name)
					continue
				}
				for _, p := range walk(name, f.Type, sub) {
					t.Error(p)
				}
			}
		})
		t.Run(ver+"/AppProject syncWindows", func(t *testing.T) {
			s := loadSchema(t, "testdata/appproject-crd-"+ver+".yaml.gz")
			spec := s["properties"].(map[string]any)["spec"].(map[string]any)
			sw := spec["properties"].(map[string]any)["syncWindows"].(map[string]any)
			var w []SyncWindow
			for _, p := range walk("spec.syncWindows", reflect.TypeOf(w), sw) {
				// syncOverrun exists from Argo CD 3.5; the older schema legitimately lacks it.
				if strings.HasSuffix(p, ".syncOverrun: not in the CRD schema") && ver == "v3.4.9" {
					continue
				}
				t.Error(p)
			}
		})
	}
	// The 3.5 schema must have syncOverrun; the 3.4 schema must not (this is why sync window rules are version dependent).
	has := func(ver string) bool {
		s := loadSchema(t, "testdata/appproject-crd-"+ver+".yaml.gz")
		sw := s["properties"].(map[string]any)["spec"].(map[string]any)["properties"].(map[string]any)["syncWindows"].(map[string]any)
		_, ok := sw["items"].(map[string]any)["properties"].(map[string]any)["syncOverrun"]
		return ok
	}
	if !has("v3.5.3") || has("v3.4.9") {
		t.Fatalf("syncOverrun: 3.5.3=%v 3.4.9=%v, expected true/false", has("v3.5.3"), has("v3.4.9"))
	}
}
