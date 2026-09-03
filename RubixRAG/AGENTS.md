# Hinweise für KI-Agenten (Claude Code, Codex, etc.)

Diese Datei bündelt Umgebungs-Eigenheiten dieses Rechners/Repos, die sonst
bei jeder neuen Session erneut mühsam entdeckt werden müssten. Bitte vor
dem ersten `go`-Befehl in einer neuen Session lesen.

## Go-Toolchain: das Standard-`go` in PATH ist zu alt

Das Standard-`go` (einfach `go version` ohne Anpassung) ist **Go 1.16** —
zu alt, um `go.mod`s `go 1.25.x`-Direktive überhaupt zu parsen. Jeder
`go build`/`go vet`/`go test`-Aufruf (und sogar `go doc` für
Nicht-Standardbibliothek-Pakete) schlägt damit sofort mit einem
"invalid go version"-Parse-Fehler fehl — das ist **kein** Zeichen für
kaputten Code, sondern schlicht der falsche Interpreter.

**Fix:** Ein funktionierendes, neueres SDK liegt unter
`C:\Users\waldherr\go-sdk\go\bin` (aktuell go1.26.4) — vor PATH stellen.
Shell-State bleibt zwischen einzelnen Tool-Aufrufen **nicht** erhalten,
daher das Voranstellen in **jedem** Befehl wiederholen (oder mit `;`/`&&`
verketten):

```powershell
$env:PATH = "C:\Users\waldherr\go-sdk\go\bin;" + $env:PATH; go build .
```

```bash
PATH="/c/Users/waldherr/go-sdk/go/bin:$PATH" go build .
```

Kurz verifizieren mit `go version` (sollte ≥ go1.23 zeigen, siehe README
"Toolchain note").

## `./...` funktioniert inzwischen wieder (external/ sind eigene Module)

Historischer Hinweis, Stand jetzt **überholt**: `external/zndz/` (und die
übrigen `external/*`-Verzeichnisse — Referenz-/Scratch-Projekte, kein Teil
von R3) brachen früher jeden rekursiven Scope (`go build ./...` usw.), weil
ihnen Fremdabhängigkeiten fehlten. Inzwischen hat **jedes**
`external/*`-Verzeichnis ein eigenes `go.mod` und wird damit von `./...`
des Hauptmoduls automatisch ausgeschlossen — `go build ./...`,
`go vet ./...`, `go test ./...` und die `make`-Targets (`build`/`vet`/
`test`/`check`) laufen wieder sauber durch (verifiziert 2026-07-09 mit
go1.26.4).

Auf das Hauptpaket zu scopen (`go build .` / `go vet .` / `go test .`)
bleibt trotzdem die schnellste Variante und ist weiterhin überall
gleichwertig, da R3 selbst nur aus dem einen Hauptpaket besteht. Sollte
`./...` doch wieder an `external/` scheitern, fehlt vermutlich einem neu
hinzugekommenen `external/`-Projekt sein eigenes `go.mod` — das dort
ergänzen statt die Make-Targets umzubauen.

## Vorsicht: Hintergrund-Tooling kann go.mod/go.sum eigenständig verändern

Beobachtet: Sobald ein funktionierender neuerer Go-Toolchain in PATH
verfügbar wird, hat Hintergrund-Tooling (vermutlich gopls / die
VS-Code-Go-Extension beim Laden des Workspace) `go.mod`/`go.sum`
eigenständig verändert — u. a. Dependency-Versionen mehrere Minor-Stufen
hochgezogen (`github.com/SimonWaldherr/tinySQL` v0.6.0 → v0.16.0), ohne
dass das explizit angefordert wurde. Ein einfaches `go build .`/`go vet .`/
`go test .` selbst hat das in Tests **nicht** ausgelöst — es scheint an
IDE-/Editor-Hintergrundprozessen zu liegen, nicht an den go-Befehlen
selbst.

Nach jedem go-Tooling-Einsatz kurz prüfen:

```
git status --short go.mod go.sum
```

Bei ungewollten Änderungen (kein bewusst angefordertes Dependency-Upgrade)
zurücksetzen:

```
git checkout -- go.mod go.sum
```

## Vorsicht: `settings.json`/`r3-data*` vor dem Löschen erst prüfen, nicht raten

Passiert: Vor einem Preview-Server-Neustart wurde als "Aufräumschritt"
`rm -f r3-data-preview* settings.json` ausgeführt, ohne vorher zu prüfen, ob
diese Dateien schon existierten. `settings.json` existierte bereits — mit
einem `storage.path`, der vom CLI-Default abwich (`r3-data-preview` statt
`r3-data`), also eindeutig echter, vorher schon lokal angepasster Zustand.
Beides ist `.gitignore`d, es gibt also **keine** Git-History, aus der sich
das rekonstruieren ließe — einmal weg, weg.

Diese Dateien/Ordner (`settings.json`, `r3-data/`, `r3-data.db`,
`r3-data-preview/`, generell alles, was `main.go` beim ersten Start selbst
erzeugt) können jederzeit echten, nicht wiederherstellbaren lokalen Zustand
enthalten, auch wenn sie "nur Laufzeit-Artefakte" sind. **Vor jedem
Löschen/Zurücksetzen** einer dieser Dateien:

1. Erst prüfen (`ls -la`, `cat`/`head`, Zeitstempel), ob Inhalt/Alter zur
   aktuellen Session passt — z. B. `storage.path`/Modell-URLs, die vom
   CLI-Default abweichen, oder ein `mtime`, das vor dem eigenen
   Session-Start liegt, sind ein starkes Signal für "das war schon da,
   nicht meins".
2. Nur löschen, was in der laufenden Session selbst erzeugt wurde (klar
   durch Zeitstempel/Inhalt erkennbar) — bei jedem Zweifel erst nachfragen
   statt anzunehmen "ist bestimmt nur ein Cleanup-Rest".
3. Kein `rm`/`rm -rf` "zur Sicherheit vor einem Neustart" ohne konkreten
   Anlass — ein neuer Serverstart braucht kein vorheriges Löschen, `main.go`
   erzeugt fehlende Dateien ohnehin automatisch mit sinnvollen Defaults.

## Sonstiges

- `docs/` ist lokal vorhanden (README/ANLEITUNG verweisen darauf), aber
  seit einer bewussten `.gitignore`-Änderung **nicht mehr Teil der
  Git-History** — Änderungen dort landen nicht im nächsten Commit. Das ist
  Absicht (u. a. wegen sensibler Beispiel-/Deployment-Inhalte), nicht
  vergessen worden.
- Kein lokaler CI-Ersatz vorhanden — vor größeren Änderungen zusätzlich
  `go build .`/`go vet .`/`go test .` laufen lassen (siehe oben), da es
  hier keinen automatischen Build-Check gibt.
