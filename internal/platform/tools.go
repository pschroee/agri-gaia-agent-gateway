package platform

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Die kuratierten Werkzeuge. MCP (Präfix platform_) und das CLI agw-platform
// erzeugen ihre Oberfläche aus dieser einen Tabelle, damit beide Anbindungen
// dieselben Beschreibungen tragen. Gebaut wird der Aufruf immer im Orchestrator.

// Param beschreibt ein Argument. Type ist ein JSON-Schema-Typ: integer, string, object oder
// array (Liste von Texten). Path: Der Wert ist ein Dateipfad in der Ausführungs-Sandbox (das CLI
// macht relative Pfade absolut).
type Param struct {
	Name     string
	Type     string
	Desc     string
	Required bool
	Path     bool
}

// Tool ist ein Werkzeug der Plattform-Anbindung.
type Tool struct {
	Name   string // ohne Präfix, etwa list_datasets
	CLI    string // Unterbefehl von agw-platform, etwa datasets
	Desc   string
	Params []Param
	// Write: Das Werkzeug schreibt und braucht eine Bestätigung (nur zur Beschreibung; maßgeblich
	// ist die Methode des gebauten Aufrufs, beim Rohzugriff also die des Agenten).
	Write bool
	build func(a Args) (Request, error)
}

// MCPName ist der Name am MCP-Server (in pi mit Präfix mcp_).
func (t Tool) MCPName() string { return "platform_" + t.Name }

// Schema ist das JSON-Schema der Eingabe für MCP.
func (t Tool) Schema() map[string]any {
	props := map[string]any{}
	req := []string{}
	for _, p := range t.Params {
		s := map[string]any{"description": p.Desc}
		if p.Type != "" {
			s["type"] = p.Type
		}
		if p.Type == "object" && p.Name == "query" {
			s["additionalProperties"] = map[string]any{"type": "string"}
		}
		if p.Type == "array" {
			s["items"] = map[string]any{"type": "string"}
		}
		props[p.Name] = s
		if p.Required {
			req = append(req, p.Name)
		}
	}
	out := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	if len(req) > 0 {
		out["required"] = req
	}
	return out
}

// Build prüft die Argumente und baut den Aufruf (bereits normalisiert).
func (t Tool) Build(raw json.RawMessage) (Request, error) {
	a := Args{}
	if len(bytes.TrimSpace(raw)) > 0 && string(bytes.TrimSpace(raw)) != "null" {
		d := json.NewDecoder(bytes.NewReader(raw))
		d.UseNumber() // große Ganzzahlen unverändert weiterreichen (Review M2)
		if err := d.Decode(&a); err != nil {
			return Request{}, fmt.Errorf("Argumente sind kein JSON-Objekt: %v", err)
		}
	}
	known := map[string]Param{}
	for _, p := range t.Params {
		known[p.Name] = p
	}
	for k := range a {
		if _, ok := known[k]; !ok {
			return Request{}, fmt.Errorf("unbekanntes Argument %q", k)
		}
	}
	for _, p := range t.Params {
		if v, ok := a[p.Name]; p.Required && (!ok || v == nil) {
			return Request{}, fmt.Errorf("Argument %q fehlt", p.Name)
		}
	}
	r, err := t.build(a)
	if err != nil {
		return Request{}, err
	}
	return Normalize(r)
}

// Args sind die Argumente eines Werkzeugaufrufs.
type Args map[string]any

var segRe = regexp.MustCompile(`^[A-Za-z0-9_\-.:~ ]+$`)

func (a Args) int(name string) (int64, bool, error) {
	v, ok := a[name]
	if !ok || v == nil {
		return 0, false, nil
	}
	switch x := v.(type) {
	case json.Number:
		n, err := x.Int64()
		if err != nil {
			return 0, false, fmt.Errorf("%s muss eine ganze Zahl sein", name)
		}
		return n, true, nil
	case float64:
		if x != float64(int64(x)) {
			return 0, false, fmt.Errorf("%s muss eine ganze Zahl sein", name)
		}
		return int64(x), true, nil
	case string:
		n, err := strconv.ParseInt(strings.TrimSpace(x), 10, 64)
		if err != nil {
			return 0, false, fmt.Errorf("%s muss eine ganze Zahl sein", name)
		}
		return n, true, nil
	}
	return 0, false, fmt.Errorf("%s muss eine ganze Zahl sein", name)
}

func (a Args) str(name string) (string, bool, error) {
	v, ok := a[name]
	if !ok || v == nil {
		return "", false, nil
	}
	s, ok := v.(string)
	if !ok {
		return "", false, fmt.Errorf("%s muss ein Text sein", name)
	}
	return s, s != "", nil
}

// seg prüft einen Wert, der in den Pfad eingesetzt wird.
func (a Args) seg(name string) (string, bool, error) {
	s, ok, err := a.str(name)
	if err != nil || !ok {
		return s, ok, err
	}
	if !segRe.MatchString(s) || s == "." || s == ".." {
		return "", false, fmt.Errorf("%s enthält unzulässige Zeichen", name)
	}
	return s, true, nil
}

func (a Args) object(name string) (json.RawMessage, bool, error) {
	v, ok := a[name]
	if !ok || v == nil {
		return nil, false, nil
	}
	if s, isStr := v.(string); isStr { // CLI und manche Modelle schicken JSON als Text
		d := json.NewDecoder(strings.NewReader(s))
		d.UseNumber()
		var x any
		if err := d.Decode(&x); err != nil || d.More() {
			return nil, false, fmt.Errorf("%s ist kein gültiges JSON", name)
		}
		v = x
	}
	if _, isObj := v.(map[string]any); !isObj {
		return nil, false, fmt.Errorf("%s muss ein JSON-Objekt sein", name)
	}
	b, err := json.Marshal(v)
	return b, true, err
}

// strings liest eine Liste von Texten (ein einzelner Text gilt als Liste mit einem Eintrag).
func (a Args) strings(name string) ([]string, error) {
	v, ok := a[name]
	if !ok || v == nil {
		return nil, nil
	}
	switch x := v.(type) {
	case string:
		return []string{x}, nil
	case []any:
		out := make([]string, 0, len(x))
		for _, e := range x {
			s, ok := e.(string)
			if !ok {
				return nil, fmt.Errorf("%s muss eine Liste von Texten sein", name)
			}
			out = append(out, s)
		}
		return out, nil
	}
	return nil, fmt.Errorf("%s muss eine Liste von Texten sein", name)
}

// keywords liest Schlagwörter; die Plattform erwartet URIs (AGROVOC), keine freien Wörter.
func keywords(a Args) ([]string, error) {
	kw, err := a.strings("keywords")
	if err != nil {
		return nil, err
	}
	for _, k := range kw {
		if !strings.HasPrefix(k, "http://") && !strings.HasPrefix(k, "https://") {
			return nil, fmt.Errorf("keywords: %q ist keine URI (AGROVOC-Begriffe über GET /agrovoc/keywords)", k)
		}
	}
	return kw, nil
}

// paging übernimmt skip und limit in die Abfrage.
func paging(a Args, r Request) (Request, error) {
	for _, k := range []string{"skip", "limit"} {
		n, ok, err := a.int(k)
		if err != nil {
			return r, err
		}
		if ok {
			if n < 0 {
				return r, fmt.Errorf("%s darf nicht negativ sein", k)
			}
			if r.Query == nil {
				r.Query = map[string]string{}
			}
			r.Query[k] = strconv.FormatInt(n, 10)
		}
	}
	return r, nil
}

var pagingParams = []Param{
	{Name: "skip", Type: "integer", Desc: "so viele Einträge überspringen (Standard 0)"},
	{Name: "limit", Type: "integer", Desc: "höchstens so viele Einträge"},
}

func get(path string) func(Args) (Request, error) {
	return func(a Args) (Request, error) { return paging(a, Request{Method: http.MethodGet, Path: path}) }
}

func byID(method, prefix, param, suffix string) func(Args) (Request, error) {
	return func(a Args) (Request, error) {
		id, ok, err := a.int(param)
		if err != nil {
			return Request{}, err
		}
		if !ok || id < 0 {
			return Request{}, fmt.Errorf("%s fehlt", param)
		}
		return Request{Method: method, Path: fmt.Sprintf("%s/%d%s", prefix, id, suffix)}, nil
	}
}

// Tools ist die Tabelle aller kuratierten Werkzeuge.
var Tools = []Tool{
	{Name: "rights", CLI: "rights", Desc: "Zeigt die Rechte, die der Nutzer dir für diesen Chat übertragen hat (welche Aktionen auf welchen Objekten der Plattform), und die Objekte, die du in diesem Chat angelegt hast. Vor schreibenden Aufrufen nachsehen; Aufrufe außerhalb dieser Rechte weist der Autorisierungsdienst ab.",
		Params: nil, build: func(Args) (Request, error) { return Request{Method: http.MethodGet, Path: RightsPath}, nil }},
	{Name: "list_datasets", CLI: "datasets", Desc: "Listet die Datensätze auf der Agri-Gaia-Plattform (id, name, owner, Beschreibung, Annotation). Die Liste enthält die Datensätze aller Nutzer.",
		Params: pagingParams, build: get("/datasets")},
	{Name: "get_dataset", CLI: "dataset", Desc: "Liefert die Metadaten eines Datensatzes der Plattform.",
		Params: []Param{{Name: "dataset_id", Type: "integer", Desc: "id des Datensatzes", Required: true}}, build: byID(http.MethodGet, "/datasets", "dataset_id", "")},
	{Name: "list_models", CLI: "models", Desc: "Listet die Modelle auf der Plattform (id, name, format, owner, labels).",
		Params: pagingParams, build: get("/models")},
	{Name: "get_model", CLI: "model", Desc: "Liefert die Metadaten eines Modells der Plattform.",
		Params: []Param{{Name: "model_id", Type: "integer", Desc: "id des Modells", Required: true}}, build: byID(http.MethodGet, "/models", "model_id", "")},
	{Name: "train_options", CLI: "train-options", Desc: "Trainingsvorlagen der Plattform. Ohne Argument: Anbieter (etwa Torchvision, Ultralytics). Mit provider: Architekturen samt category. Mit provider und architecture: JSON-Schema und Standardwerte der train_config für platform_create_training.",
		Params: []Param{{Name: "provider", Type: "string", Desc: "Anbieter, etwa Torchvision"}, {Name: "architecture", Type: "string", Desc: "Architektur, etwa EfficientNet (nur mit provider)"}},
		build: func(a Args) (Request, error) {
			p, hasP, err := a.seg("provider")
			if err != nil {
				return Request{}, err
			}
			arch, hasA, err := a.seg("architecture")
			if err != nil {
				return Request{}, err
			}
			switch {
			case hasA && !hasP:
				return Request{}, errors.New("architecture nur zusammen mit provider")
			case hasA:
				return Request{Method: http.MethodGet, Path: "/train/config/" + p + "/" + arch}, nil
			case hasP:
				return Request{Method: http.MethodGet, Path: "/train/architectures/" + p}, nil
			}
			return Request{Method: http.MethodGet, Path: "/train/providers"}, nil
		}},
	{Name: "list_trainings", CLI: "trainings", Desc: "Listet die Trainingscontainer (id, provider, architecture, dataset_id, status, score). Hinweis: Die Plattform gleicht dabei den Status mit Docker ab und entfernt Einträge, deren Container verschwunden ist; das gilt hier als Lesen und braucht keine Bestätigung.",
		Params: pagingParams, build: get("/train/containers")},
	{Name: "get_training", CLI: "training", Desc: "Liefert einen Trainingscontainer.",
		Params: []Param{{Name: "train_container_id", Type: "integer", Desc: "id des Trainingscontainers", Required: true}}, build: byID(http.MethodGet, "/train/containers", "train_container_id", "")},
	{Name: "training_status", CLI: "training-status", Desc: "Status und Score eines Trainingscontainers.",
		Params: []Param{{Name: "train_container_id", Type: "integer", Desc: "id des Trainingscontainers", Required: true}}, build: byID(http.MethodGet, "/train/containers", "train_container_id", "/status")},
	{Name: "training_logs", CLI: "training-logs", Desc: "Ende des Protokolls (Konsolenausgabe) eines Trainingscontainers, standardmäßig die letzten 200 Zeilen.",
		Params: []Param{{Name: "train_container_id", Type: "integer", Desc: "id des Trainingscontainers", Required: true}, {Name: "tail", Type: "integer", Desc: "die letzten N Zeilen (Standard 200)"}},
		build: func(a Args) (Request, error) {
			r, err := byID(http.MethodGet, "/train/containers", "train_container_id", "/logs")(a)
			if err != nil {
				return r, err
			}
			// Ohne tail liefert das Backend alles, gekürzt würde dann der Anfang statt des Endes (Review M3).
			n, ok, err := a.int("tail")
			if err != nil {
				return r, err
			}
			if !ok {
				n = 200
			}
			if n <= 0 {
				return r, errors.New("tail muss positiv sein")
			}
			r.Query = map[string]string{"tail": strconv.FormatInt(n, 10)}
			return r, nil
		}},
	{Name: "list_tasks", CLI: "tasks", Desc: "Listet Hintergrundaufgaben der Plattform (Bau und Start von Trainingscontainern u. a.) mit status, completion_percentage und message.",
		Params: pagingParams, build: get("/tasks")},
	{Name: "task_status", CLI: "task", Desc: "Liefert eine Hintergrundaufgabe der Plattform (status: inprogress, completed, failed; completion_percentage; message).",
		Params: []Param{{Name: "task_id", Type: "integer", Desc: "id der Aufgabe (steht in der Location-Angabe von platform_create_training und platform_start_training)", Required: true}}, build: byID(http.MethodGet, "/tasks", "task_id", "")},
	{Name: "list_edge_devices", CLI: "edge-devices", Desc: "Listet die Edge-Geräte der Plattform.",
		Params: nil, build: get("/edge-devices")},
	{Name: "list_container_images", CLI: "container-images", Desc: "Listet die Container-Abbilder in der Registry der Plattform.",
		Params: nil, build: get("/container-images")},
	{Name: "create_training", CLI: "create-training", Desc: "Legt einen Trainingscontainer an (POST /train/config): Die Plattform baut im Hintergrund ein Trainingsabbild aus der installierten Vorlage. Schreibt; der Nutzer muss bestätigen. Antwort: Location mit der Aufgabe (/tasks/<id>). Werte für train_config vorher mit platform_train_options holen.",
		Params: []Param{
			{Name: "provider", Type: "string", Desc: "Anbieter, etwa Torchvision", Required: true},
			{Name: "architecture", Type: "string", Desc: "Architektur, etwa EfficientNet", Required: true},
			{Name: "category", Type: "string", Desc: "Kategorie der Architektur laut platform_train_options, etwa Classification", Required: true},
			{Name: "dataset_id", Type: "integer", Desc: "id des Datensatzes", Required: true},
			{Name: "train_config", Type: "object", Desc: "Trainingsparameter nach dem Schema der Vorlage (values aus platform_train_options als Ausgangspunkt)", Required: true},
			{Name: "export_config", Type: "object", Desc: "Einstellungen für den ONNX-Export; weglassen für keinen Export"},
		},
		Write: true,
		build: func(a Args) (Request, error) {
			body := map[string]any{}
			for _, k := range []string{"provider", "architecture", "category"} {
				s, ok, err := a.str(k)
				if err != nil {
					return Request{}, err
				}
				if !ok {
					return Request{}, fmt.Errorf("%s fehlt", k)
				}
				body[k] = s
			}
			id, ok, err := a.int("dataset_id")
			if err != nil || !ok {
				return Request{}, errors.Join(errors.New("dataset_id fehlt oder ist keine Zahl"), err)
			}
			body["dataset_id"] = id
			tc, ok, err := a.object("train_config")
			if err != nil || !ok {
				return Request{}, errors.Join(errors.New("train_config fehlt"), err)
			}
			body["train_config"] = tc
			ec, ok, err := a.object("export_config")
			if err != nil {
				return Request{}, err
			}
			if ok {
				body["export_config"] = ec
			} else {
				body["export_config"] = nil
			}
			b, err := json.Marshal(body)
			return Request{Method: http.MethodPost, Path: "/train/config", Body: b}, err
		}},
	{Name: "start_training", CLI: "start-training", Desc: "Startet einen gebauten Trainingscontainer (POST /train/containers/<id>/run). Schreibt; der Nutzer muss bestätigen. Antwort: Location mit der Aufgabe (/tasks/<id>).",
		Params: []Param{{Name: "train_container_id", Type: "integer", Desc: "id des Trainingscontainers", Required: true}}, Write: true, build: byID(http.MethodPost, "/train/containers", "train_container_id", "/run")},
	{Name: "upload_dataset", CLI: "upload-dataset", Desc: "Legt einen Datensatz auf der Plattform an und lädt Dateien aus der Sandbox hoch (POST /datasets, multipart). Schreibt; der Nutzer muss bestätigen und sieht dabei Namen, Größe und SHA-256 jeder Datei. Klassen als annotation_labels angeben; eine CVAT-Annotation (annotations.xml) als annotation_file.",
		Params: []Param{
			{Name: "name", Type: "string", Desc: "Name des Datensatzes (eindeutig, ohne Leerzeichen am besten)", Required: true},
			{Name: "description", Type: "string", Desc: "kurze Beschreibung", Required: true},
			{Name: "files", Type: "array", Desc: "Dateipfade in der Sandbox, etwa /workspace/bilder/a.png", Required: true, Path: true},
			{Name: "annotation_file", Type: "string", Desc: "Pfad einer CVAT-Annotation (annotations.xml), wird als letzte Datei gesendet", Path: true},
			{Name: "annotation_labels", Type: "array", Desc: "Klassen (Labels) des Datensatzes, etwa [\"0\",\"1\"]"},
			{Name: "dataset_type", Type: "string", Desc: "Datensatztyp (Standard AgriImageDataResource; nur dieser lässt sich annotieren)"},
			{Name: "metadata", Type: "object", Desc: "weitere Metadaten als Objekt (Standard {}); die Plattform legt sie im Triplestore ab"},
			{Name: "keywords", Type: "array", Desc: "Schlagwörter als AGROVOC-URIs (Suche: platform_request GET /agrovoc/keywords?keyword=…)"},
		},
		Write: true,
		build: func(a Args) (Request, error) {
			form := map[string][]string{"is_classification_dataset": {"false"}} // siehe docs/plattform-testdurchlauf.md: true verwirft die Labels
			for _, k := range []string{"name", "description"} {
				v, ok, err := a.str(k)
				if err != nil {
					return Request{}, err
				}
				if !ok {
					return Request{}, fmt.Errorf("%s fehlt", k)
				}
				form[k] = []string{v}
			}
			dt, ok, err := a.str("dataset_type")
			if err != nil {
				return Request{}, err
			}
			if !ok {
				dt = "AgriImageDataResource"
			}
			form["dataset_type"] = []string{dt}
			// Ohne metadata scheitert das Backend am Triplestore (json.loads(None), HTTP 500, am
			// 05.10.2026 an der Instanz gesehen); die Web-UI schickt immer mindestens {}.
			md, ok, err := a.object("metadata")
			if err != nil {
				return Request{}, err
			}
			if !ok {
				md = json.RawMessage(`{}`)
			}
			form["metadata"] = []string{string(md)}
			kw, err := keywords(a)
			if err != nil {
				return Request{}, err
			}
			if len(kw) > 0 {
				form["semantic_labels"] = kw
			}
			labels, err := a.strings("annotation_labels")
			if err != nil {
				return Request{}, err
			}
			if len(labels) > 0 {
				form["annotation_labels"] = labels
			}
			paths, err := a.strings("files")
			if err != nil {
				return Request{}, err
			}
			if len(paths) == 0 {
				return Request{}, errors.New("files: mindestens eine Datei")
			}
			files := make([]File, 0, len(paths)+1)
			for _, p := range paths {
				files = append(files, File{Field: "files", Path: p})
			}
			ann, hasAnn, err := a.str("annotation_file")
			if err != nil {
				return Request{}, err
			}
			form["includes_annotation_file"] = []string{strconv.FormatBool(hasAnn)}
			if hasAnn {
				files = append(files, File{Field: "files", Path: ann})
			}
			return Request{Method: http.MethodPost, Path: "/datasets", Form: form, Files: files}, nil
		}},
	{Name: "upload_model", CLI: "upload-model", Desc: "Lädt ein Modell aus der Sandbox auf die Plattform (POST /models, multipart). Schreibt; der Nutzer muss bestätigen und sieht Name, Größe und SHA-256 der Datei. Bei ONNX liest die Plattform Ein- und Ausgabeformen selbst aus.",
		Params: []Param{
			{Name: "name", Type: "string", Desc: "Name des Modells", Required: true},
			{Name: "description", Type: "string", Desc: "kurze Beschreibung", Required: true},
			{Name: "format", Type: "string", Desc: "onnx, pytorch, tensorflow oder tensorrt", Required: true},
			{Name: "model_file", Type: "string", Desc: "Pfad der Modelldatei in der Sandbox, etwa /workspace/model.onnx", Required: true, Path: true},
			{Name: "keywords", Type: "array", Desc: "Schlagwörter als AGROVOC-URIs (Suche: platform_request GET /agrovoc/keywords?keyword=…); keine Klassennamen"},
		},
		Write: true,
		build: func(a Args) (Request, error) {
			form := map[string][]string{}
			for _, k := range []string{"name", "description", "format"} {
				v, ok, err := a.str(k)
				if err != nil {
					return Request{}, err
				}
				if !ok {
					return Request{}, fmt.Errorf("%s fehlt", k)
				}
				form[k] = []string{v}
			}
			switch form["format"][0] {
			case "onnx", "pytorch", "tensorflow", "tensorrt":
			default:
				return Request{}, errors.New("format muss onnx, pytorch, tensorflow oder tensorrt sein")
			}
			// Das Formularfeld heißt labels, trägt in der Web-UI aber AGROVOC-URIs für den Triplestore
			// (UploadModelDialog.tsx); Klassennamen landeten dort als ungültige URIs (05.10.2026).
			kw, err := keywords(a)
			if err != nil {
				return Request{}, err
			}
			if len(kw) > 0 {
				form["labels"] = kw
			}
			mf, ok, err := a.str("model_file")
			if err != nil || !ok {
				return Request{}, errors.Join(errors.New("model_file fehlt"), err)
			}
			return Request{Method: http.MethodPost, Path: "/models", Form: form, Files: []File{{Field: "modelfile", Path: mf}}}, nil
		}},
	{Name: "api_paths", CLI: "api-paths", Desc: "Verzeichnis aller Pfade der REST-API der Plattform aus deren OpenAPI-Beschreibung: je Zeile Methode, Pfad, Zweck, Parameter und Art des Körpers. Mit prefix nur Pfade, die so beginnen (etwa /train). Grundlage für platform_request.",
		Params: []Param{{Name: "prefix", Type: "string", Desc: "nur Pfade mit diesem Anfang, etwa /datasets"}},
		build: func(a Args) (Request, error) {
			prefix, _, err := a.str("prefix")
			if err != nil {
				return Request{}, err
			}
			return Request{Method: http.MethodGet, Path: "/openapi.json", Digest: func(b []byte) ([]byte, error) { return openAPIDigest(b, prefix) }}, nil
		}},
	{Name: "request", CLI: "request", Desc: "Beliebiger Aufruf der REST-API der Plattform für alles, was die anderen platform_-Werkzeuge nicht abdecken (Pfade: platform_api_paths). GET geht direkt (außer bekannten GETs, die im Backend schreiben, etwa /train/containers/<id>/model); POST, PUT, PATCH und DELETE muss der Nutzer bestätigen. Werte mit Passwörtern, Schlüsseln und Tokens schwärzt der Orchestrator. Nur JSON-Körper, keine Datei-Uploads.",
		Params: []Param{
			{Name: "method", Type: "string", Desc: "GET, POST, PUT, PATCH oder DELETE", Required: true},
			{Name: "path", Type: "string", Desc: "Pfad relativ zur API, etwa /datasets/3 (ohne Abfrage)", Required: true},
			{Name: "query", Type: "object", Desc: "Abfrageparameter als Objekt mit Textwerten, etwa {\"limit\":\"10\"}"},
			{Name: "body", Desc: "JSON-Körper (nur bei POST, PUT, PATCH, DELETE)"},
		},
		build: func(a Args) (Request, error) {
			m, _, err := a.str("method")
			if err != nil {
				return Request{}, err
			}
			p, _, err := a.str("path")
			if err != nil {
				return Request{}, err
			}
			r := Request{Method: m, Path: p}
			if q, ok := a["query"]; ok && q != nil {
				qm, isObj := q.(map[string]any)
				if !isObj {
					return r, errors.New("query muss ein Objekt sein")
				}
				r.Query = map[string]string{}
				for k, v := range qm {
					switch x := v.(type) {
					case string:
						r.Query[k] = x
					case json.Number, float64, bool:
						r.Query[k] = fmt.Sprint(x)
					default:
						return r, fmt.Errorf("query.%s muss ein Text sein", k)
					}
				}
			}
			if b, ok := a["body"]; ok && b != nil {
				raw, err := json.Marshal(b)
				if err != nil {
					return r, err
				}
				r.Body = raw
			}
			return r, nil
		}},
}

// Lookup findet ein Werkzeug nach Name (mit oder ohne Präfix platform_) oder CLI-Name.
func Lookup(name string) (Tool, bool) {
	name = strings.TrimPrefix(name, "platform_")
	for _, t := range Tools {
		if t.Name == name || t.CLI == name {
			return t, true
		}
	}
	return Tool{}, false
}

// openAPIDigest verdichtet eine OpenAPI-Beschreibung zu einer Zeile je Operation.
func openAPIDigest(raw []byte, prefix string) ([]byte, error) {
	var doc struct {
		Paths map[string]map[string]struct {
			Summary    string `json:"summary"`
			Parameters []struct {
				Name     string `json:"name"`
				In       string `json:"in"`
				Required bool   `json:"required"`
			} `json:"parameters"`
			RequestBody *struct {
				Content map[string]json.RawMessage `json:"content"`
			} `json:"requestBody"`
		} `json:"paths"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("OpenAPI-Beschreibung unlesbar: %w", err)
	}
	paths := make([]string, 0, len(doc.Paths))
	for p := range doc.Paths {
		if strings.HasPrefix(p, prefix) {
			paths = append(paths, p)
		}
	}
	sort.Strings(paths)
	var b strings.Builder
	order := []string{"get", "post", "put", "patch", "delete"}
	n := 0
	for _, p := range paths {
		for _, m := range order {
			op, ok := doc.Paths[p][m]
			if !ok {
				continue
			}
			n++
			fmt.Fprintf(&b, "%s %s — %s", strings.ToUpper(m), p, op.Summary)
			var q []string
			for _, par := range op.Parameters {
				if par.In == "query" {
					s := par.Name
					if par.Required {
						s += "*"
					}
					q = append(q, s)
				}
			}
			if len(q) > 0 {
				fmt.Fprintf(&b, " · query: %s", strings.Join(q, ", "))
			}
			if op.RequestBody != nil {
				ct := make([]string, 0, len(op.RequestBody.Content))
				for k := range op.RequestBody.Content {
					ct = append(ct, k)
				}
				sort.Strings(ct)
				fmt.Fprintf(&b, " · Körper: %s", strings.Join(ct, ", "))
			}
			b.WriteString("\n")
		}
	}
	fmt.Fprintf(&b, "(%d Operationen; * = Pflicht; Körper multipart/form-data geht über platform_request nicht)\n", n)
	return []byte(b.String()), nil
}
