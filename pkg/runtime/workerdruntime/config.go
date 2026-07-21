// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

package workerdruntime

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/durupages/durupages/pkg/api"
	"github.com/durupages/durupages/pkg/runtime"
)

// DefaultCompatDate is the compatibility date used when a page's manifest does
// not carry one. It is a recent, stable Cloudflare Workers compatibility date.
const DefaultCompatDate = "2024-09-23"

// Names of the generated (trusted) worker services and the file names their
// modules are written to inside the per-instance directory.
const (
	entryService  = "entry"
	tailService   = "tailworker"
	tailOutbound  = "tail_out"
	entryModule   = "entry.js"
	tailModule    = "tail.js"
	routesBinding = "ROUTES"
	assetsBinding = "ASSETS"
	tailEndptBind = "TAIL_ENDPOINT"
)

// generated holds the text artifacts produced for one instance: the workerd
// config plus the two generated worker scripts. They are written into the
// per-instance directory and referenced from the config via `embed`.
type generated struct {
	Capnp   []byte
	EntryJS []byte
	TailJS  []byte
}

// moduleKind classifies how a bundle file is declared to workerd.
type moduleKind int

const (
	modESModule moduleKind = iota
	modWasm
	modText
	modData
)

// module is one worker module: its import specifier (Name), the embed path
// relative to the config directory, and how it is declared.
type module struct {
	Name  string
	Embed string
	Kind  moduleKind
}

// pageBuild is the resolved config input for one page worker.
type pageBuild struct {
	name    string // page_<i> service name
	svc     string // svc_<i> entry binding name
	assets  string // assets_<i> external service name
	page    runtime.PageWorker
	modules []module
}

// generateConfig renders the workerd config and the generated worker scripts
// for spec, with the entry socket listening on 127.0.0.1:port. Output is
// deterministic: pages keep spec order, bindings and the route map are sorted.
func generateConfig(spec runtime.InstanceSpec, port int) (*generated, error) {
	assetsAddr := spec.AssetsEndpoint
	tailAddr := spec.TailEndpoint

	// Resolve every page's modules first so a bundle error aborts before we
	// emit any text.
	builds := make([]pageBuild, len(spec.Pages))
	routes := make(map[string]string, len(spec.Pages))
	for i, p := range spec.Pages {
		embedPrefix := fmt.Sprintf("page_%d/worker", i)
		mods, err := discoverModules(filepath.Join(p.BundleDir, "worker"), embedPrefix)
		if err != nil {
			return nil, fmt.Errorf("workerdruntime: page %q: %w", p.PageID, err)
		}
		if len(mods) == 0 {
			return nil, fmt.Errorf("workerdruntime: page %q: no worker modules found", p.PageID)
		}
		svc := fmt.Sprintf("svc_%d", i)
		builds[i] = pageBuild{
			name:    fmt.Sprintf("page_%d", i),
			svc:     svc,
			assets:  fmt.Sprintf("assets_%d", i),
			page:    p,
			modules: mods,
		}
		routes[p.PageID] = svc
	}

	routesJSON, err := json.Marshal(routes)
	if err != nil {
		return nil, fmt.Errorf("workerdruntime: encode routes: %w", err)
	}

	var b strings.Builder
	b.WriteString("using Workerd = import \"/workerd/workerd.capnp\";\n\n")
	b.WriteString("const config :Workerd.Config = (\n")
	b.WriteString("  services = [\n")

	var svcs []string

	// entry dispatcher (trusted).
	var entry strings.Builder
	writeServiceOpen(&entry, entryService)
	entry.WriteString("      worker = (\n")
	writeModules(&entry, []module{{Name: entryModule, Embed: entryModule, Kind: modESModule}})
	fmt.Fprintf(&entry, "        compatibilityDate = %s,\n", capnpString(DefaultCompatDate))
	entry.WriteString("        bindings = [\n")
	fmt.Fprintf(&entry, "          ( name = %s, json = %s )", capnpString(routesBinding), capnpString(string(routesJSON)))
	for _, pb := range builds {
		entry.WriteString(",\n")
		fmt.Fprintf(&entry, "          ( name = %s, service = %s )", capnpString(pb.svc), capnpString(pb.name))
	}
	entry.WriteString("\n        ]\n")
	entry.WriteString("      )\n")
	entry.WriteString("    )")
	svcs = append(svcs, entry.String())

	// tail worker (trusted) — its global outbound is bound to an external
	// service so its loopback POST to the shim collector is permitted.
	var tail strings.Builder
	writeServiceOpen(&tail, tailService)
	tail.WriteString("      worker = (\n")
	writeModules(&tail, []module{{Name: tailModule, Embed: tailModule, Kind: modESModule}})
	fmt.Fprintf(&tail, "        compatibilityDate = %s,\n", capnpString(DefaultCompatDate))
	tail.WriteString("        bindings = [\n")
	fmt.Fprintf(&tail, "          ( name = %s, text = %s )\n", capnpString(tailEndptBind), capnpString(endpointURL(tailAddr)))
	tail.WriteString("        ],\n")
	fmt.Fprintf(&tail, "        globalOutbound = %s\n", capnpString(tailOutbound))
	tail.WriteString("      )\n")
	tail.WriteString("    )")
	svcs = append(svcs, tail.String())

	// external service the tail worker's global outbound points at.
	svcs = append(svcs, externalService(tailOutbound, tailAddr, ""))

	// one page worker + one assets external service per page.
	for _, pb := range builds {
		var s strings.Builder
		writeServiceOpen(&s, pb.name)
		s.WriteString("      worker = (\n")
		writeModules(&s, pb.modules)
		fmt.Fprintf(&s, "        compatibilityDate = %s,\n", capnpString(compatDate(pb.page)))
		if flags := compatFlags(pb.page); len(flags) > 0 {
			s.WriteString("        compatibilityFlags = [")
			for j, f := range flags {
				if j > 0 {
					s.WriteString(", ")
				}
				s.WriteString(capnpString(f))
			}
			s.WriteString("],\n")
		}
		s.WriteString("        bindings = [\n")
		for _, kv := range mergedBindings(pb.page) {
			fmt.Fprintf(&s, "          ( name = %s, text = %s ),\n", capnpString(kv.key), capnpString(kv.val))
		}
		fmt.Fprintf(&s, "          ( name = %s, service = %s )\n", capnpString(assetsBinding), capnpString(pb.assets))
		s.WriteString("        ],\n")
		fmt.Fprintf(&s, "        tails = [%s]\n", capnpString(tailService))
		s.WriteString("      )\n")
		s.WriteString("    )")
		svcs = append(svcs, s.String())

		inject := fmt.Sprintf("          injectRequestHeaders = [\n            ( name = %s, value = %s )\n          ]",
			capnpString(api.HeaderPage), capnpString(pb.page.PageID))
		svcs = append(svcs, externalService(pb.assets, assetsAddr, inject))
	}

	b.WriteString(strings.Join(svcs, ",\n"))
	b.WriteString("\n  ],\n")
	b.WriteString("  sockets = [\n")
	fmt.Fprintf(&b, "    ( name = \"http\", address = %s, http = (), service = %s )\n",
		capnpString(fmt.Sprintf("127.0.0.1:%d", port)), capnpString(entryService))
	b.WriteString("  ]\n")
	b.WriteString(");\n")

	return &generated{
		Capnp:   []byte(b.String()),
		EntryJS: []byte(entryWorkerJS),
		TailJS:  []byte(tailWorkerJS),
	}, nil
}

func writeServiceOpen(b *strings.Builder, name string) {
	b.WriteString("    (\n")
	fmt.Fprintf(b, "      name = %s,\n", capnpString(name))
}

func writeModules(b *strings.Builder, mods []module) {
	b.WriteString("        modules = [\n")
	for i, m := range mods {
		if i > 0 {
			b.WriteString(",\n")
		}
		fmt.Fprintf(b, "          ( name = %s, %s = embed %s )",
			capnpString(m.Name), moduleField(m.Kind), capnpString(m.Embed))
	}
	b.WriteString("\n        ],\n")
}

func moduleField(k moduleKind) string {
	switch k {
	case modWasm:
		return "wasmModule"
	case modText:
		return "text"
	case modData:
		return "data"
	default:
		return "esModule"
	}
}

// externalService renders an `external` service pointing at addr (host:port).
// extraHTTP, when non-empty, is inserted inside the http(...) group.
func externalService(name, addr, extraHTTP string) string {
	var s strings.Builder
	writeServiceOpen(&s, name)
	s.WriteString("      external = (\n")
	fmt.Fprintf(&s, "        address = %s,\n", capnpString(addr))
	if extraHTTP == "" {
		s.WriteString("        http = ()\n")
	} else {
		s.WriteString("        http = (\n")
		s.WriteString(extraHTTP)
		s.WriteString("\n        )\n")
	}
	s.WriteString("      )\n")
	s.WriteString("    )")
	return s.String()
}

// endpointURL turns a host:port loopback endpoint into an http:// URL with a
// trailing slash, suitable for the tail worker's fetch target.
func endpointURL(hostPort string) string {
	return "http://" + hostPort + "/"
}

func compatDate(p runtime.PageWorker) string {
	if p.Manifest != nil && p.Manifest.Compat.Date != "" {
		return p.Manifest.Compat.Date
	}
	return DefaultCompatDate
}

func compatFlags(p runtime.PageWorker) []string {
	if p.Manifest == nil {
		return nil
	}
	return p.Manifest.Compat.Flags
}

type binding struct{ key, val string }

// mergedBindings returns Env and Secret as one sorted binding list. Env and
// Secret are exposed identically (the contract); Secret wins on a key clash.
func mergedBindings(p runtime.PageWorker) []binding {
	merged := make(map[string]string, len(p.Env)+len(p.Secret))
	for k, v := range p.Env {
		merged[k] = v
	}
	for k, v := range p.Secret {
		merged[k] = v
	}
	keys := make([]string, 0, len(merged))
	for k := range merged {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]binding, 0, len(keys))
	for _, k := range keys {
		out = append(out, binding{key: k, val: merged[k]})
	}
	return out
}

// discoverModules walks workerDir and returns its modules with worker/index.js
// (the entry module) first, followed by the rest in lexical order. Module names
// are POSIX paths relative to workerDir; embed paths are prefixed with
// embedPrefix (which is relative to the config directory).
func discoverModules(workerDir, embedPrefix string) ([]module, error) {
	var mods []module
	err := filepath.WalkDir(workerDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, rerr := filepath.Rel(workerDir, p)
		if rerr != nil {
			return rerr
		}
		rel = filepath.ToSlash(rel)
		mods = append(mods, module{
			Name:  rel,
			Embed: path.Join(embedPrefix, rel),
			Kind:  moduleKindFor(rel),
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(mods, func(i, j int) bool {
		ei, ej := mods[i].Name == "index.js", mods[j].Name == "index.js"
		if ei != ej {
			return ei // index.js sorts first (it is the entry module)
		}
		return mods[i].Name < mods[j].Name
	})
	return mods, nil
}

func moduleKindFor(name string) moduleKind {
	switch strings.ToLower(path.Ext(name)) {
	case ".js", ".mjs", ".cjs":
		return modESModule
	case ".wasm":
		return modWasm
	case ".txt":
		return modText
	default:
		return modData
	}
}

// capnpString renders s as a capnp text-format string literal (double-quoted,
// with C-style escapes). It is used for both plain values and the embedded
// JSON/JS payloads, so it must escape control characters conservatively.
func capnpString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if c < 0x20 || c == 0x7f {
				fmt.Fprintf(&b, `\x%02x`, c)
			} else {
				b.WriteByte(c)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}
