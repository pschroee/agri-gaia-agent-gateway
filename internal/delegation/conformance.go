package delegation

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"agw/internal/platform"
)

// Konformitätsprüfung ohne Sprachmodell (Gliederung 7.3.1): jeder verbotene Aufruf mit feindlichen
// Argumenten direkt an den Autorisierungsdienst, auch in Varianten, die ein Parser anders verstehen
// könnte; dazu erlaubte Kontrollaufrufe, damit eine Prüfung, die alles abweist, nicht als korrekt
// durchgeht. Soll: alle verbotenen abgewiesen, alle erlaubten durchgelassen.

// ConformanceDelegation ist die Delegation, gegen die geprüft wird: Datensätze lesen und anlegen,
// eigene ändern; Trainingscontainer 7 lesen und starten; eigene Aufgaben lesen; Vorlagen lesen.
const ConformanceDelegation = `{
  "rules": [
    {"action": "read",   "resource": "dataset", "ids": ["*"]},
    {"action": "create", "resource": "dataset"},
    {"action": "update", "resource": "dataset", "ids": ["own"]},
    {"action": "read",   "resource": "training", "ids": ["7"]},
    {"action": "run",    "resource": "training", "ids": ["7"]},
    {"action": "read",   "resource": "task", "ids": ["own"]},
    {"action": "read",   "resource": "train_template"}
  ],
  "confirm": "none"
}`

// OwnInConformance sind die Objekte, die in der geprüften Delegation entstanden sind.
var OwnInConformance = []string{"dataset:12", "task:40"}

// Case ist ein Prüffall: entweder ein Werkzeug mit Argumenten oder ein Rohaufruf.
type Case struct {
	Name    string
	Tool    string // Werkzeug aus platform.Tools (leer: Req)
	Args    string
	Req     platform.Request
	Expired bool // Delegation abgelaufen
	Allowed bool // Soll
	Group   string
	Rules   string // eigene Delegation statt ConformanceDelegation (leer: diese)
}

// ConformanceCases liefert die Prüffälle.
func ConformanceCases() []Case {
	req := func(m, p string) platform.Request { return platform.Request{Method: m, Path: p} }
	body := func(m, p, b string) platform.Request {
		return platform.Request{Method: m, Path: p, Body: json.RawMessage(b)}
	}
	return []Case{
		// Erlaubte Kontrollaufrufe
		{Name: "Datensätze auflisten", Tool: "list_datasets", Allowed: true, Group: "Kontrolle"},
		{Name: "fremden Datensatz lesen (read *)", Tool: "get_dataset", Args: `{"dataset_id":5}`, Allowed: true, Group: "Kontrolle"},
		{Name: "Datensatz anlegen", Req: platform.Request{Method: "POST", Path: "/datasets", Form: map[string][]string{"name": {"x"}}}, Allowed: true, Group: "Kontrolle"},
		{Name: "eigenen Datensatz ändern", Req: body("PATCH", "/datasets/12", `{"description":"neu"}`), Allowed: true, Group: "Kontrolle"},
		{Name: "Protokoll von Container 7", Tool: "training_logs", Args: `{"train_container_id":7}`, Allowed: true, Group: "Kontrolle"},
		{Name: "Container 7 starten", Tool: "start_training", Args: `{"train_container_id":7}`, Allowed: true, Group: "Kontrolle"},
		{Name: "eigene Aufgabe lesen", Tool: "task_status", Args: `{"task_id":40}`, Allowed: true, Group: "Kontrolle"},
		{Name: "Vorlagen lesen", Tool: "train_options", Args: `{"provider":"Torchvision"}`, Allowed: true, Group: "Kontrolle"},
		{Name: "API-Beschreibung lesen", Tool: "api_paths", Allowed: true, Group: "Kontrolle"},

		// Aktion oder Ressource nicht übertragen
		{Name: "fremden Datensatz löschen", Req: body("DELETE", "/datasets/5", `{"key":"-","csrftoken":"-","sessionid":"-"}`), Group: "Aktion"},
		{Name: "fremden Datensatz ändern", Req: body("PATCH", "/datasets/5", `{"owner":"test"}`), Group: "Aktion"},
		{Name: "fremden Datensatz öffentlich schalten", Req: req("PATCH", "/datasets/5/toggle-public"), Group: "Aktion"},
		{Name: "fremden Datensatz annotieren", Req: req("POST", "/datasets/5/annotate"), Group: "Aktion"},
		{Name: "Modelle auflisten (keine Regel)", Tool: "list_models", Group: "Ressource"},
		{Name: "Modell hochladen (keine Regel)", Req: platform.Request{Method: "POST", Path: "/models", Form: map[string][]string{"name": {"m"}}}, Group: "Ressource"},
		{Name: "Training anlegen (keine Regel)", Tool: "create_training", Args: `{"provider":"T","architecture":"E","category":"C","dataset_id":12,"train_config":{}}`, Group: "Ressource"},
		{Name: "Container 8 starten", Tool: "start_training", Args: `{"train_container_id":8}`, Group: "Objekt"},
		{Name: "fremde Aufgabe löschen", Req: req("DELETE", "/tasks/3"), Group: "Objekt"},
		{Name: "fremde Aufgabe lesen", Tool: "task_status", Args: `{"task_id":3}`, Group: "Objekt"},
		{Name: "Edge-Geräte auflisten (keine Regel)", Tool: "list_edge_devices", Group: "Ressource"},

		// Varianten, die ein Parser anders verstehen könnte
		{Name: "Kennung mit führender Null (08)", Req: req("POST", "/train/containers/08/run"), Group: "Parser"},
		{Name: "Kennung mit Leerzeichen ( 5)", Req: body("PATCH", "/datasets/ 5", `{}`), Group: "Parser"},
		{Name: "angehängter Schrägstrich", Req: body("PATCH", "/datasets/5/", `{}`), Group: "Parser"},
		{Name: "Großschreibung im Pfad", Req: body("PATCH", "/Datasets/5", `{}`), Group: "Parser"},
		{Name: "Methode klein geschrieben über request", Tool: "request", Args: `{"method":"patch","path":"/datasets/5","body":{"name":"x"}}`, Group: "Parser"},
		{Name: "doppelte Methode im JSON (letzte gilt)", Tool: "request", Args: `{"method":"GET","method":"DELETE","path":"/datasets/5"}`, Group: "Parser"},
		{Name: "doppelte container_id, fremde zuletzt", Req: body("PUT", "/train/config", `{"container_id":7,"container_id":8,"train_config":{},"export_config":null,"dataset_id":12}`), Group: "Parser"},
		{Name: "container_id als Text", Req: body("PUT", "/train/config", `{"container_id":"8","train_config":{},"export_config":null,"dataset_id":12}`), Group: "Parser"},
		{Name: "Prozentkodierung im Pfad", Req: req("DELETE", "/datasets/5%2F"), Group: "Parser"},
		{Name: "Pfad mit ..", Req: req("GET", "/datasets/../models/1"), Group: "Parser"},
		{Name: "doppelter Schrägstrich", Req: req("GET", "//models"), Group: "Parser"},
		{Name: "absolute URL als Pfad", Req: req("GET", "https://evil.example/models"), Group: "Parser"},
		{Name: "Abfrage im Pfad", Req: req("GET", "/models?skip=0"), Group: "Parser"},

		// Schreibende GETs und gesperrte Bereiche
		{Name: "GET legt Modell aus Container 7 an", Req: req("GET", "/train/containers/7/model"), Group: "Schreibendes GET"},
		{Name: "GET startet Lizenzanalyse", Req: platform.Request{Method: "GET", Path: "/licenses/", Query: map[string]string{"return_cached": "false"}}, Group: "Schreibendes GET"},
		{Name: "Fuseki-Administratorzugang", Req: req("GET", "/urls/basic-auth"), Group: "Gesperrt"},
		{Name: "EDC-Passwort", Req: req("GET", "/network/info"), Group: "Gesperrt"},
		{Name: "Profil des Kontos", Req: req("GET", "/users/me"), Group: "Gesperrt"},
		{Name: "Rückruf der Registry", Req: req("POST", "/service/registry-event"), Group: "Gesperrt"},

		// Ablauf und Herkunft
		{Name: "lesen nach Ablauf der Delegation", Tool: "list_datasets", Expired: true, Group: "Ablauf"},
		{Name: "eigenen Datensatz ändern nach Ablauf", Req: body("PATCH", "/datasets/12", `{}`), Expired: true, Group: "Ablauf"},
		{Name: "Datensatz 13 als eigen ausgeben (nicht im Register)", Req: body("PATCH", "/datasets/13", `{}`), Group: "Herkunft"},
		{Name: "Container 7 ändern (nur lesen und starten erlaubt)", Req: body("PATCH", "/train/containers/7", `{}`), Group: "Herkunft"},

		// Felder und Kennungen im Körper (Review 5: K1, K2, W1, W2)
		{Name: "eigenen Datensatz umbenennen (erlaubtes Feld)", Req: body("PATCH", "/datasets/12", `{"name":"neu","description":"x"}`), Allowed: true, Group: "Feld"},
		{Name: "eigener Datensatz: bucket_name setzen", Req: body("PATCH", "/datasets/12", `{"bucket_name":"x; id #"}`), Group: "Feld"},
		{Name: "eigener Datensatz: metadata_uri setzen", Req: body("PATCH", "/datasets/12", `{"metadata_uri":"https://fremd#_Dataset"}`), Group: "Feld"},
		{Name: "eigener Datensatz: owner setzen", Req: body("PATCH", "/datasets/12", `{"name":"x","owner":"anderer"}`), Group: "Feld"},
		{Name: "eigener Datensatz: Körper ist kein Objekt", Req: body("PATCH", "/datasets/12", `["name"]`), Group: "Feld"},
		{Name: "Modell aus fremdem Container 8", Req: req("GET", "/train/containers/8/model"), Group: "Kennung im Körper"},
		{Name: "Training auf fremdem Datensatz (Leserecht fehlt)", Req: body("POST", "/train/config", `{"provider":"T","dataset_id":99}`), Expired: false, Group: "Kennung im Körper", Rules: `{"rules":[{"action":"create","resource":"training"},{"action":"read","resource":"dataset","ids":["12"]}],"confirm":"none"}`},
		{Name: "Training auf eigenem Datensatz (Kontrolle)", Req: body("POST", "/train/config", `{"provider":"T","dataset_id":12}`), Allowed: true, Group: "Kennung im Körper", Rules: `{"rules":[{"action":"create","resource":"training"},{"action":"read","resource":"dataset","ids":["12"]}],"confirm":"none"}`},
		{Name: "PUT /train/config mit fremdem Datensatz", Req: body("PUT", "/train/config", `{"container_id":7,"dataset_id":99}`), Group: "Kennung im Körper", Rules: `{"rules":[{"action":"update","resource":"training","ids":["7"]},{"action":"read","resource":"dataset","ids":["12"]}],"confirm":"none"}`},
		{Name: "container_id jenseits von float64 (…993 gegen Regel …992)", Req: body("PUT", "/train/config", `{"container_id":9007199254740993,"dataset_id":12}`), Group: "Kennung im Körper", Rules: `{"rules":[{"action":"update","resource":"training","ids":["9007199254740992"]},{"action":"read","resource":"dataset","ids":["*"]}],"confirm":"none"}`},
		{Name: "container_id als 7.0", Req: body("PUT", "/train/config", `{"container_id":7.0,"dataset_id":12}`), Group: "Kennung im Körper"},
		{Name: "container_id als Liste", Req: body("PUT", "/train/config", `{"container_id":[7],"dataset_id":12}`), Group: "Kennung im Körper"},
		{Name: "Kennung 012 eines eigenen Datensatzes (Kontrolle)", Req: body("PATCH", "/datasets/012", `{"name":"x"}`), Allowed: true, Group: "Parser"},

		// Weitere schreibende GETs und Ressourcen (Review 5: W5, M2)
		{Name: "GET Download schreibt Annotation nach MinIO", Req: req("GET", "/datasets/5/download"), Group: "Schreibendes GET"},
		{Name: "GET registriert Edge-Gerät", Req: req("GET", "/edge-devices/3"), Group: "Schreibendes GET"},
		{Name: "Edge-Gerät löschen", Req: req("DELETE", "/edge-devices/3"), Group: "Ressource"},
		{Name: "unbekannter Pfad löschen", Req: req("DELETE", "/integrated-services/3"), Group: "Ressource"},
		{Name: "Methode HEAD", Req: req("HEAD", "/datasets"), Group: "Parser"},
	}
}

// Build macht aus einem Fall einen geprüften Aufruf. Ein Fehler heißt: schon die Prüfung der
// Argumente weist ab (zählt als abgewiesen).
func (c Case) Build() (platform.Request, error) {
	if c.Tool != "" {
		t, ok := platform.Lookup(c.Tool)
		if !ok {
			return platform.Request{}, fmt.Errorf("Werkzeug %s fehlt", c.Tool)
		}
		return t.Build(json.RawMessage(c.Args))
	}
	return platform.Normalize(c.Req)
}

// Evaluate prüft einen Fall gegen ConformanceDelegation (ohne Plattform). allowed: der Dienst ließe
// den Aufruf durch.
func Evaluate(c Case, now time.Time) (allowed bool, why string) {
	raw := ConformanceDelegation
	if c.Rules != "" {
		raw = c.Rules
	}
	d, err := Parse([]byte(raw))
	if err != nil {
		return false, err.Error()
	}
	if c.Expired {
		past := now.Add(-time.Minute)
		d.ExpiresAt = &past
	}
	req, err := c.Build()
	if err != nil {
		return false, "abgewiesen bei der Prüfung der Argumente: " + err.Error()
	}
	own := func(res, id string) bool {
		for _, o := range OwnInConformance {
			if o == res+":"+id {
				return true
			}
		}
		return false
	}
	dec := d.Check(Classify(req), now, own)
	if !dec.Allowed {
		return false, dec.Reason
	}
	return true, "erlaubt: " + dec.Access.String()
}

// Report fasst Ergebnisse als Tabelle zusammen (Markdown, für 7.3.1).
func Report(results map[string]bool, cases []Case) string {
	var b strings.Builder
	b.WriteString("| Gruppe | Fälle | Soll erfüllt |\n|---|---|---|\n")
	type agg struct{ n, ok int }
	order := []string{}
	groups := map[string]*agg{}
	for _, c := range cases {
		g, found := groups[c.Group]
		if !found {
			g = &agg{}
			groups[c.Group] = g
			order = append(order, c.Group)
		}
		g.n++
		if results[c.Name] == c.Allowed {
			g.ok++
		}
	}
	for _, name := range order {
		g := groups[name]
		fmt.Fprintf(&b, "| %s | %d | %d |\n", name, g.n, g.ok)
	}
	return b.String()
}
