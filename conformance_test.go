package zapsmtp_test

// Helpers of the conformance test (zz_generated_conformance_test.go): the
// conformance server (sdks/conformance/server.mjs) checks every request; these
// check that every response decoded into its model, field by field.

import (
	"bufio"
	"encoding/json"
	"fmt"
	"iter"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/zapsmtp/zapsmtp-go"
)

const conformanceDir = "../conformance"

var (
	conformanceURL   string // the server's base URL, or "" when it is not available
	conformanceCases map[string]conformanceCase
)

type conformanceCase struct {
	ID       string `json:"id"`
	Response struct {
		Status int             `json:"status"`
		Body   json.RawMessage `json:"body"`
	} `json:"response"`
}

func TestMain(m *testing.M) {
	stop, err := startConformanceServer()
	if err != nil {
		fmt.Fprintln(os.Stderr, "conformance server:", err)
		os.Exit(1)
	}
	code := m.Run()
	stop()
	os.Exit(code)
}

// startConformanceServer uses ZAP_CONFORMANCE_URL when it is set, else starts
// node ../conformance/server.mjs. Outside this repository (no server.mjs) the
// conformance tests skip.
func startConformanceServer() (func(), error) {
	raw, err := os.ReadFile(filepath.Join(conformanceDir, "cases.json"))
	if os.IsNotExist(err) {
		return func() {}, nil
	}
	if err != nil {
		return nil, err
	}
	var cases []conformanceCase
	if err := json.Unmarshal(raw, &cases); err != nil {
		return nil, err
	}
	conformanceCases = map[string]conformanceCase{}
	for _, c := range cases {
		conformanceCases[c.ID] = c
	}
	if u := os.Getenv("ZAP_CONFORMANCE_URL"); u != "" {
		conformanceURL = u
		return func() {}, nil
	}
	cmd := exec.Command("node", filepath.Join(conformanceDir, "server.mjs"), "0")
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting node (needed for the conformance tests): %w", err)
	}
	line, err := bufio.NewReader(out).ReadString('\n')
	m := regexp.MustCompile(`listening (\d+)`).FindStringSubmatch(line)
	if m == nil {
		_ = cmd.Process.Kill()
		return nil, fmt.Errorf("unexpected output %q (%v)", line, err)
	}
	conformanceURL = "http://127.0.0.1:" + m[1] + "/v1"
	return func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }, nil
}

func conformanceClient(t *testing.T) *zapsmtp.Client {
	t.Helper()
	if conformanceCases == nil {
		t.Skip("no sdks/conformance next to this module")
	}
	return zapsmtp.NewClient("zap_test_conformance", zapsmtp.WithBaseURL(conformanceURL), zapsmtp.WithMaxRetries(0))
}

func caseBody(t *testing.T, id string) []byte {
	t.Helper()
	c, ok := conformanceCases[id]
	if !ok {
		t.Fatalf("no case %s in cases.json: regenerate (node sdks/codegen/generate.mjs)", id)
	}
	return c.Response.Body
}

func checkRanAll(t *testing.T, ran map[string]bool) {
	t.Helper()
	for id := range conformanceCases {
		if !ran[id] {
			t.Errorf("case %s did not run: regenerate (node sdks/codegen/generate.mjs)", id)
		}
	}
	if len(ran) != len(conformanceCases) {
		t.Errorf("ran %d cases, cases.json has %d", len(ran), len(conformanceCases))
	}
}

func checkNoContent(t *testing.T, id string, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	if b := caseBody(t, id); string(b) != "null" {
		t.Fatalf("the case answers %s, the method returns nothing", b)
	}
}

func checkResult(t *testing.T, id string, res any, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	checkDecoded(t, res, caseBody(t, id))
}

func checkPage[T any](t *testing.T, id string, page *zapsmtp.Page[T], err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	want := caseBody(t, id)
	checkDecoded(t, page, want)
	checkJSONEqual(t, "Raw", page.Raw, want)
}

// checkAutoPaging walks the first page's items (the next page would be a
// request the case does not describe).
func checkAutoPaging[T any](t *testing.T, id string, seq iter.Seq2[T, error]) {
	t.Helper()
	var body struct {
		Data []json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(caseBody(t, id), &body); err != nil {
		t.Fatal(err)
	}
	n := 0
	for item, err := range seq {
		if err != nil {
			t.Fatal(err)
		}
		checkDecoded(t, &item, body.Data[n])
		if n++; n == len(body.Data) {
			break
		}
	}
	if n != len(body.Data) {
		t.Fatalf("auto-paging gave %d items, want %d", n, len(body.Data))
	}
}

func checkJSONEqual(t *testing.T, what string, got, want []byte) {
	t.Helper()
	var g, w any
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatalf("%s: %v", what, err)
	}
	_ = json.Unmarshal(want, &w)
	if !reflect.DeepEqual(g, w) {
		t.Fatalf("%s: got %s, want %s", what, got, want)
	}
}

// checkDecoded checks that v (a pointer to a model) holds every field of the
// JSON want, with its value, and that its Raw is want.
func checkDecoded(t *testing.T, v any, want []byte) {
	t.Helper()
	var j any
	if err := json.Unmarshal(want, &j); err != nil {
		t.Fatal(err)
	}
	var errs []string
	compare("", reflect.ValueOf(v), j, &errs)
	if len(errs) > 0 {
		t.Fatalf("decoded value differs from the JSON:\n%s", strings.Join(errs, "\n"))
	}
}

var timeType = reflect.TypeFor[time.Time]()

func compare(path string, v reflect.Value, j any, errs *[]string) {
	fail := func(format string, args ...any) { *errs = append(*errs, path+": "+fmt.Sprintf(format, args...)) }
	if j == nil {
		if !v.IsZero() {
			fail("got %v for null", v.Interface())
		}
		return
	}
	for v.Kind() == reflect.Pointer {
		if v.IsNil() {
			fail("nil for %v", j)
			return
		}
		v = v.Elem()
	}
	if v.Type() == timeType {
		s, _ := j.(string)
		want, err := time.Parse(time.RFC3339Nano, s)
		if err != nil || !v.Interface().(time.Time).Equal(want) {
			fail("got %v, want %v", v.Interface(), j)
		}
		return
	}
	switch v.Kind() {
	case reflect.Interface:
		if !reflect.DeepEqual(v.Interface(), j) {
			fail("got %#v, want %#v", v.Interface(), j)
		}
	case reflect.Struct:
		m, ok := j.(map[string]any)
		if !ok {
			fail("got an object for %v", j)
			return
		}
		fields := map[string]int{}
		for i := range v.NumField() {
			f := v.Type().Field(i)
			if name, _, _ := strings.Cut(f.Tag.Get("json"), ","); name != "" && name != "-" && f.IsExported() {
				fields[name] = i
			}
			if f.Name == "Raw" {
				var r any
				if err := json.Unmarshal(v.Field(i).Bytes(), &r); err != nil || !reflect.DeepEqual(r, j) {
					fail("Raw is %s", v.Field(i).Bytes())
				}
			}
		}
		for k, x := range m {
			i, ok := fields[k]
			if !ok {
				fail("no field for %q", k)
				continue
			}
			compare(path+"."+k, v.Field(i), x, errs)
		}
	case reflect.Slice:
		a, ok := j.([]any)
		if !ok || len(a) != v.Len() {
			fail("got %d items for %v", v.Len(), j)
			return
		}
		for i := range a {
			compare(fmt.Sprintf("%s[%d]", path, i), v.Index(i), a[i], errs)
		}
	case reflect.Map:
		m, ok := j.(map[string]any)
		if !ok || len(m) != v.Len() {
			fail("got %d entries for %v", v.Len(), j)
			return
		}
		for k, x := range m {
			e := v.MapIndex(reflect.ValueOf(k))
			if !e.IsValid() {
				fail("no entry %q", k)
				continue
			}
			compare(path+"["+k+"]", e, x, errs)
		}
	case reflect.String:
		if v.String() != j {
			fail("got %q, want %v", v.String(), j)
		}
	case reflect.Bool:
		if v.Bool() != j {
			fail("got %v, want %v", v.Bool(), j)
		}
	case reflect.Int:
		if f, ok := j.(float64); !ok || float64(v.Int()) != f {
			fail("got %d, want %v", v.Int(), j)
		}
	case reflect.Float64:
		if f, ok := j.(float64); !ok || v.Float() != f {
			fail("got %v, want %v", v.Float(), j)
		}
	default:
		fail("unexpected kind %s", v.Kind())
	}
}
