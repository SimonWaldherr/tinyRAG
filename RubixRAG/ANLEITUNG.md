# R3 — Anleitung

**Status:** R3 läuft als MVP bereits produktiv unter
`http://svde-vld-ai01.zitec-intern.de:8090/`, installiert unter
`/mnt/application/R3` auf diesem Server. Änderungen an dieser Installation
(neues Binary, neue `settings.json`-Felder) betreffen diesen laufenden
Dienst — vor einem Neustart dort das Vorgehen aus Abschnitt 6
("Aktualisieren / Warten") beachten.

Diese Anleitung fasst alles zusammen, was zum **Betreiben**, **Konfigurieren**,
**Aktualisieren** und **Erweitern** von R3 (Rubix Ranked RAG) nötig ist — auf
Deutsch, damit sie ohne Vorwissen aus dem restlichen (englischen)
Quellcode-Kommentar-Stil nutzbar ist. Für tiefere technische Hintergründe zu
einzelnen Entscheidungen verweist diese Anleitung auf `README.md` und die
Dateien in `docs/`, die auf Englisch bleiben (Konsistenz mit dem restlichen
Code-Kommentarstil). Diese Anleitung richtet sich an alle, die R3
tatsächlich bedienen/konfigurieren; für einen nicht-technischen Überblick
(Management) siehe [`LIESMICH.md`](LIESMICH.md) und
[`docs/PROJEKTPLAN.md`](docs/PROJEKTPLAN.md).

## Inhalt

1. [Was ist R3?](#was-ist-r3)
2. [Voraussetzungen](#voraussetzungen)
3. [R3 starten](#r3-starten)
4. [Konfiguration (`settings.json`)](#konfiguration-settingsjson)
5. [Bedienung der Web-Oberfläche](#bedienung-der-web-oberfläche)
6. [Aktualisieren / Warten](#aktualisieren--warten)
7. [R3 erweitern](#r3-erweitern)
8. [Fehlerbehebung](#fehlerbehebung)
9. [Sicherheit & Datenschutz — kurz](#sicherheit--datenschutz--kurz)
10. [Weiterführende Dokumente](#weiterführende-dokumente)

---

## Was ist R3?

R3 ist ein **RAG-System** (Retrieval-Augmented Generation) für
E-Mail-Postfächer und Dokumentenbestände: Es importiert Inhalte (PST-Export
inkl. Anhänge, Dateien, SharePoint, Exchange, IMAP, Teams, Confluence, Jira,
Webseiten), zerlegt sie in Textabschnitte ("Chunks"), berechnet dafür
Embeddings und beantwortet Fragen im Browser mit **belegten
Quellenangaben** — jede Antwort verweist auf die konkreten Chunks, aus denen
sie stammt (und nur auf die davon tatsächlich verwendeten, siehe
"Zitat-Sichtbarkeit je Quelltyp" in Abschnitt 4).

Technisch ist R3 **ein einziges Go-Binary** ohne externe Datenbank: Die
Vektorsuche läuft eingebettet (tinySQL oder SQLite, beides reines Go ohne
CGO), das Frontend ist eingebettetes HTML/CSS/JavaScript ohne Build-Schritt.
Das macht Auslieferung und Betrieb einfach: eine Datei kopieren, starten,
fertig.

## Voraussetzungen

- **Go ≥ 1.23** zum Bauen (siehe `go.mod`; auf dem Rubix-Zielserver bereits
  mit Go ≥1.23 gebaut/getestet — siehe README.md "Toolchain note" für
  Hintergründe zu einer älteren Go-1.16-Umgebung, falls das relevant wird).
- **Ein LLM-Backend**, entweder:
  - ein lokaler, OpenAI-kompatibler Server (LM Studio, Ollama, vLLM, …) mit
    einem Chat- und einem Embedding-Modell geladen, oder
  - Azure OpenAI Service (Chat- + Embedding-Deployment).

  Ohne erreichbares Backend startet R3 trotzdem (nur eine Warnung im Log),
  aber Fragen/Import schlagen fehl, bis eines erreichbar ist.
- **Optional, je nach genutzten Funktionen:** `markitdown` (Office-/
  PDF-Import), `tesseract` (Bild-OCR), `ffmpeg` (Audio-Konvertierung für
  Whisper) und/oder `whisper.cpp` (lokale Sprache-zu-Text-Transkription,
  "Voice-Mode") — siehe den eigenen Abschnitt unten für die konkrete
  Installation, insbesondere unter Debian. Jede dieser Funktionen braucht
  zusätzlich `allow_shell_exec` in den Einstellungen (siehe unten), da R3
  sonst nie einen externen Prozess startet.
- **Optional, je nach genutzten Connectoren:** Zugangsdaten für Active
  Directory (LDAP), eine Azure-AD-App-Registrierung (SharePoint/Exchange/
  Teams), einen Confluence-API-Token, einen SQL-Server-Zugang — siehe
  Abschnitt 4.

### Abhängigkeiten installieren (insbesondere Debian)

Alle folgenden Programme sind rein optional — R3 startet und beantwortet
Fragen zu bereits importierten Quellen auch ganz ohne sie. Sie werden nur
zur Laufzeit für die jeweilige Funktion nachgefragt (nie beim Start selbst)
und ausschließlich, wenn `allow_shell_exec` aktiv ist; ist ein Programm
nicht installiert oder deaktiviert, schlägt nur die jeweilige einzelne
Aktion (Datei-Import, Bild-OCR, Sprachaufnahme) mit einer klaren
Fehlermeldung fehl, der Rest von R3 bleibt unberührt.

**Go-Toolchain.** Debians eigenes `golang-go`-Paket ist auf `stable` oft zu
alt für `go.mod`s Anforderung (dieses Repo dokumentiert in `AGENTS.md`
selbst einen Fall, in dem ein vorhandenes Go 1.16 die `go.mod`-Direktive
gar nicht erst parsen konnte) — im Zweifel ein aktuelles Release direkt von
[go.dev/dl](https://go.dev/dl/) als Tarball installieren, statt sich auf das
Distributions-Paket zu verlassen:

```bash
curl -LO https://go.dev/dl/go1.26.5.linux-amd64.tar.gz
sudo rm -rf /usr/local/go && sudo tar -C /usr/local -xzf go1.26.5.linux-amd64.tar.gz
echo 'export PATH=$PATH:/usr/local/go/bin' >> ~/.bashrc   # oder /etc/profile.d/ für alle Nutzer
source ~/.bashrc
go version   # sollte >= 1.23 zeigen
```

**Ein lokales LLM-Backend ohne grafische Oberfläche.** LM Studio setzt
klassischerweise eine Desktop-Oberfläche voraus; auf einem reinen
Debian-Server ohne GUI bietet sich stattdessen z. B.
[Ollama](https://ollama.com) an (eigenes Install-Skript, kein
Debian-Paket) — es spricht seit einiger Zeit dieselbe OpenAI-kompatible
`/v1/...`-API, die R3 über `-url`/`profiles.local.base_url` anspricht:

```bash
curl -fsSL https://ollama.com/install.sh | sh
ollama pull <chat-modell>       # z. B. mistral-nemo, llama3.1
ollama pull <embedding-modell>  # z. B. nomic-embed-text
```

**`markitdown`** (Office-/PDF-/RTF-/ODT-/EPUB-Import, optional Audio/Video
via `markitdown[all]`):

```bash
sudo apt install python3-pip pipx
pipx install markitdown           # oder: pipx install 'markitdown[all]'
```

Debian 12/13 verweigern seit der PEP-668-Umstellung ein System-weites
`pip install <paket>` standardmäßig (`error: externally-managed-
environment`) — `pipx` (isolierte Umgebung pro Programm, landet automatisch
im `PATH`) umgeht das sauber. Alternativ funktioniert auch
`pip install --break-system-packages markitdown` oder eine eigene
`python3 -m venv`-Umgebung.

**`tesseract`** (OCR für Bild-Uploads/-Anhänge — Scans, fotografierte
Dokumente):

```bash
sudo apt install tesseract-ocr tesseract-ocr-deu
```

`tesseract-ocr-eng` ist im Basispaket bereits enthalten (Standard-Sprache
`deu+eng`); weitere Sprachpakete nach Bedarf, z. B. `tesseract-ocr-fra`.

**`ffmpeg`** (Audio-Normalisierung für Voice-Mode, außerdem für
`markitdown[all]`s Video-Unterstützung):

```bash
sudo apt install ffmpeg
```

**`whisper.cpp`** (lokale Sprache-zu-Text-Transkription, "Voice-Mode" —
kein Debian-Paket, muss aus dem Quellcode gebaut werden):

```bash
sudo apt install build-essential cmake git
git clone https://github.com/ggml-org/whisper.cpp.git
cd whisper.cpp
cmake -B build && cmake --build build -j --config Release
sudo install -m 0755 build/bin/whisper-cli /usr/local/bin/whisper-cli
```

Zusätzlich ein Modell herunterladen (bleibt vollständig lokal, wird von R3
selbst **nie** heruntergeladen):

```bash
./models/download-ggml-model.sh small   # oder tiny/base/medium/large-v3, je nach RAM/Genauigkeit
```

Anschließend in den Einstellungen unter "Import" `whisper_bin` (Pfad zu
`whisper-cli`, falls nicht im `PATH`), `whisper_model` (Pfad zur
heruntergeladenen `.bin`-Datei) und optional `whisper_language` (z. B.
`de`) eintragen — siehe "Import (`import`)" weiter unten für die
zusätzlichen Tuning-Felder (Threads, Beam-Size, VAD, Flash-Attention,
maximale Parallelität).

## R3 starten

### Schnellstart

```bash
go build .
./R3 -addr :8090 -url http://localhost:1234 \
     -chat mistralai/ministral-3-3b -embed text-embedding-nomic-embed-text-v1.5
```

Danach `http://localhost:8090` im Browser öffnen. `-url`/`-chat`/`-embed`
zeigen auf den lokalen LLM-Server; ohne Angabe werden diese Standardwerte
genutzt (siehe Flag-Tabelle unten).

### Über das Makefile (empfohlen unter Linux/macOS/WSL)

`make` ohne Argument öffnet ein interaktives Menü (Pfeiltasten + Enter).
Direkt aufrufbare Ziele:

| Befehl | Wirkung |
|---|---|
| `make build` | `go build ./...` |
| `make run` | Server starten (still, keine Request-Logs) |
| `make dev` | `fmt` + `vet`, dann Server mit `-verbose` (jeder Request und jeder LLM-Call wird geloggt) |
| `make test` | `go test ./...` |
| `make check` | `fmt` + `vet` + `test` — vor jedem Commit sinnvoll |
| `make fmt` / `make vet` / `make tidy` | einzelne Schritte |
| `make migrate MIGRATE_FROM_BACKEND=... MIGRATE_FROM_PATH=...` | Speicher-Backend wechseln, siehe Abschnitt 6 |
| `make snapshot` | Projektverzeichnis (ohne Speicherordner) als ZIP/TAR sichern |
| `make help` | Kurzübersicht direkt im Terminal |

Variablen wie `APP_ADDR`, `APP_STORAGE`, `STORAGE_BACKEND` lassen sich pro
Aufruf überschreiben, z. B. `make dev STORAGE_BACKEND=sqlite
APP_STORAGE=r3-data.db`.

### Wichtige CLI-Flags (nur beim allerersten Start relevant)

Die meisten Flags schreiben nur die **Erstwerte** in `settings.json` — nach
dem ersten Start ändert man sie über die Datei selbst oder die
Einstellungen-Seite, nicht mehr über die Flags (siehe Abschnitt 4).

| Flag | Standard | Bedeutung |
|---|---|---|
| `-addr` | `:8090` | Adresse/Port des Webservers |
| `-settings` | `settings.json` | Pfad zur Konfigurationsdatei |
| `-storage-backend` | `tinysql` | `tinysql` oder `sqlite` — **nur beim ersten Start**, siehe Abschnitt 6 |
| `-storage-path` | `r3-data` | Speicherort (Ordner bei tinySQL disk/index/hybrid, Datei bei sqlite/tinySQL memory/wal) |
| `-storage-mode` | `hybrid` | nur tinySQL: `memory`\|`wal`\|`disk`\|`index`\|`hybrid` |
| `-url` | `http://localhost:1234` | Basis-URL des lokalen LLM-Servers |
| `-chat` / `-embed` | Beispielmodelle | Modellnamen des lokalen Servers |
| `-azure-url` / `-azure-chat-deployment` / `-azure-embed-deployment` / `-azure-api-version` | leer / — / — / `2024-10-21` | Azure-OpenAI-Zugangsdaten (Schlüssel selbst kommt aus einer Umgebungsvariable, siehe Abschnitt 4) |
| `-lang` | `de` | UI-Sprache beim allerersten Start (`de`/`en`/`fr`/`it` übersetzt); danach über Einstellungen → Allgemein änderbar, siehe „Übersetzungen (i18n) erweitern" |
| `-chunk` | `800` | Chunk-Größe beim Zerlegen von Texten |
| `-k` | `5` | Anzahl Chunks, die als Kontext an das Chat-Modell gehen |
| `-verbose` | aus | jeden Request und jeden LLM-Call loggen — außerdem jeden Import-/Ingest-Schritt (siehe unten) |
| `-migrate-from-backend` / `-migrate-from-path` / `-migrate-from-mode` | — | einmalige Migration zwischen Speicher-Backends, siehe Abschnitt 6 |

Vollständige, stets aktuelle Liste: `./R3 -h`.

`-verbose` deckt auch jeden Import-Connector ab (PST, SharePoint, Exchange,
IMAP, Teams, Confluence, Jira, Freshservice, Web, Server-Ordner): pro
abgerufenem Element wird eine Zeile geloggt (Anzahl, Dry-Run-Flag), und
`ingestDocument` (die zentrale Ingest-Funktion) loggt pro Dokument eine
Zeile (Chunk-/Redaction-Anzahl, Skip-Entscheidung, Dry-Run-Flag) — praktisch
zur Fehlersuche ("warum wurde das nicht neu eingebettet", "warum 0
Chunks"), ohne einen eigenen Debug-Schalter zu brauchen.

### Als Dienst betreiben (`/etc/init.d/r3`)

Statt R3 von Hand zu starten (z. B. per `nohup`/Terminal-Session), liegt im
Repo unter `deploy/` ein fertiges init.d-Setup, das `build-linux-deploy.bat`
automatisch mit ins Deploy-Paket (`upload/`) legt:

- `deploy/r3.init` — ein LSB-init-Skript (`start`/`stop`/`restart`/`status`/
  `logs`/`debug`)
- `deploy/r3.default` — die zugehörige Konfiguration, die das Skript aus
  `/etc/default/r3` liest (Pfade, Start-Flags, Verbose-Schalter, User)

Einmalige Einrichtung auf dem Zielserver:

```bash
cp deploy/r3.init /etc/init.d/r3 && chmod +x /etc/init.d/r3
cp deploy/r3.default /etc/default/r3   # danach Pfade/Flags dort anpassen
chown root:root /etc/default/r3 && chmod 600 /etc/default/r3   # kann Secrets enthalten
update-rc.d r3 defaults                 # beim Booten automatisch starten
service r3 start
```

Danach reicht für jedes Binary-Update: `service r3 stop`, neues `R3`
hochkopieren, `service r3 start` — `deploy/r3.init`/`r3.default` müssen
dabei nicht erneut kopiert werden (nur wenn sich das Skript/die Vorlage
selbst ändert).

**Verbose-Logging an-/ausschalten, ohne den Start-Befehl anzufassen:** in
`/etc/default/r3` `R3_VERBOSE=1` setzen (0 für still), dann
`service r3 restart`. Das Skript kapselt R3 so, dass die Log-Ausgabe (R3
loggt nur über das Standard-`log`-Paket auf stderr, kein eigenes
Datei-Logging) in `R3_LOGFILE` landet (Default:
`/mnt/application/R3/r3.log`). `service r3 status` zeigt, ob der Prozess
läuft.

Zwei weitere Aktionen neben `start`/`stop`/`restart`/`status`:

- `service r3 logs` — `tail -F` auf das aktuelle `R3_LOGFILE`, egal ob
  `R3_VERBOSE` an oder aus ist und egal ob R3 gerade läuft (praktisch, um
  zu sehen, was direkt vor einem Absturz passiert ist).
- `service r3 debug` — stoppt R3 (falls es läuft), startet es neu mit
  erzwungenem `-verbose` nur für diesen einen Lauf (rührt `R3_VERBOSE` in
  `/etc/default/r3` nicht an — ein späteres normales `restart` geht wieder
  in den bisherigen Modus zurück) und hängt sich direkt an das Log.
  Strg+C beendet nur das Mitlesen, nicht den Dienst — `service r3 restart`
  schaltet danach wieder auf still.

### Beenden

`Strg+C` (SIGINT) oder `SIGTERM` lösen ein geordnetes Herunterfahren aus: R3
sichert offene Änderungen im Speicher-Backend, bevor der Prozess endet.

## Konfiguration (`settings.json`)

Beim ersten Start erzeugt R3 automatisch eine `settings.json` mit
Standardwerten. Danach gibt es **zwei gleichwertige Wege**, sie zu ändern:

1. **Web-Oberfläche** → Tab „Einstellungen" (Admin-Zugang nötig, siehe
   Abschnitt 5) — Änderungen wirken sofort, ohne Neustart, außer beim
   `storage`-Block (siehe unten).
2. **Datei direkt bearbeiten** und R3 neu starten (oder für die meisten
   Felder: einfach per `POST /api/settings` senden, siehe `docs/API.md`).

Nachfolgend jedes Feld, gruppiert nach Bereich. Felder mit *(env-Variable
bevorzugt)* haben zusätzlich ein `*_env`-Gegenstück (z. B.
`client_secret_env`), das den Namen einer Umgebungsvariable enthält, aus der
das eigentliche Geheimnis gelesen wird — das ist der empfohlene Weg,
Passwörter/Tokens *nicht* im Klartext in `settings.json` abzulegen.

**Aktualisieren nach einem Update:** Eine `settings.json` aus einer älteren
R3-Version muss vor dem Laden nicht händisch angepasst werden. Beim Start
füllt R3 jedes Feld, das die Datei noch nicht kennt — egal auf welcher
Verschachtelungsebene, auch ein komplett neuer Connector-Block —
automatisch mit dem aktuellen Standardwert auf und schreibt die
vervollständigte Datei sofort zurück. Bereits in der Datei vorhandene
Werte bleiben dabei unverändert erhalten, nur wirklich fehlende Felder
werden ergänzt.

### LLM-Backends (`profiles`, `embed_profile`, `chat_profile`)

Zwei Profile existieren parallel: `local` (ein OpenAI-kompatibler Server) und
`azure` (Azure OpenAI Service).

```json
"profiles": {
  "local": { "provider": "local", "base_url": "http://localhost:1234",
             "chat_model": "...", "embed_model": "..." },
  "azure":  { "provider": "azure", "base_url": "https://MEIN-RESOURCE.openai.azure.com",
              "chat_model": "gpt-4o", "embed_model": "text-embedding-3-large",
              "api_version": "2024-10-21", "api_key_env": "AZURE_OPENAI_API_KEY" }
},
"embed_profile": "local",
"chat_profile": "local"
```

- `embed_profile` sollte fast immer `"local"` bleiben — Embedding-Aufrufe
  sind häufig, sollen nicht bei jedem Import Cloud-Kosten/Latenz erzeugen.
- `chat_profile` ist der **Standard**; jede einzelne Frage in der Web-UI kann
  ihn über das Dropdown "LLM-Backend für diese Frage" überschreiben.
- Der Azure-API-Schlüssel gehört in eine Umgebungsvariable (Name über
  `api_key_env`, Standard `AZURE_OPENAI_API_KEY`) statt in die Datei.
- `upload` (eigener Block neben `profiles`, nicht Teil eines einzelnen
  Profils): steuert zentral, wie ein in Chat/Agent hochgeladenes
  Bild/Foto/Scan verarbeitet wird — bewusst eine explizite
  Admin-Entscheidung, keine automatische Erkennung anhand des
  Modellnamens.
  - `image_mode`: `"ocr"` (Standard) — Bild wird lokal per `tesseract`
    (siehe „Import"-Abschnitt) in Text umgewandelt, kein LLM-Aufruf,
    keine Zusatzkosten. `"vision"` — die **komplette** Chat-/Agent-Frage
    wird an das unten festgelegte `vision_profile` geschickt, **egal**
    welches Profil im Chat-/Agent-Dropdown gerade ausgewählt ist (ein
    Vision-fähiges Backend wie Azure `gpt-4o` ist oft nicht dasselbe wie
    das für normalen Text-Chat).
  - `vision_profile`: `"local"` oder `"azure"` — welches Profil
    Bild-Uploads bekommt, wenn `image_mode` `"vision"` ist. Ist
    `image_mode` `"vision"`, aber hier nichts gesetzt, oder verlangt die
    Gast-Azure-Regel (`ldap.guest_azure_profile_policy`) eine Anmeldung,
    die für diese Anfrage fehlt, wird das Bild **ignoriert** (mit einem
    Hinweis an das Modell) statt die Frage fehlschlagen zu lassen oder
    stillschweigend auf OCR umzuschalten.
  - `vision_max_dim` (Standard `0` = 1600px, einstellbar 800–1600): auf
    diese längste Kante (in Pixeln) wird ein für Vision hochgeladenes
    Bild vor dem Versand verkleinert. Wirkt nur auf den Vision-Pfad — der
    OCR-Pfad bekommt weiterhin die Originalauflösung, da `tesseract` von
    mehr Pixeln profitiert. Kleiner = weniger Kosten/Latenz pro Anfrage,
    aber irgendwann sichtbar schlechtere Lesbarkeit von kleiner Schrift
    auf Scans — 800px ist die untere Grenze, ab der das spürbar wird.
  - `vision_jpeg_quality` (Standard `0` = 85, einstellbar 50–95): JPEG-
    Qualität, mit der ein verkleinertes Bild neu kodiert wird. Niedriger
    spart mehr Bandbreite, kann aber feine Details verschlechtern.
  - `max_attachment_mb` (Standard `0` = 8 MB, einstellbar 1–50): maximale
    Größe eines einzelnen Chat/Agent-Anhangs (Bild **oder** Dokument) vor
    der Base64-Kodierung — ein größerer Anhang wird mit Fehlermeldung
    abgelehnt, nicht abgeschnitten.
  - `max_prompt_chars` (Standard `0` = 20000, einstellbar 2000–100000):
    maximale Länge einer einzelnen Chat/Agent-Frage in Zeichen — eine
    längere Frage wird ebenfalls mit Fehlermeldung abgelehnt statt
    gekürzt. Schützt vor einem versehentlich riesigen Textblock als Frage,
    nicht vor einer tatsächlich langen, aber sinnvollen.
  In der Settings-UI liegen alle sechs Felder zusammen in einer eigenen Box
  „Anhänge & Eingabelänge (Chat/Agent)" im Chat-Abschnitt (Bild/Datei/
  Prompt-Länge als Unterüberschriften), nicht mehr unter „Routing". Siehe
  „Bild-Uploads in Chat/Agent" unten für die Nutzerseite.

### Chat- & Antwortverhalten

| Feld | Standard | Bedeutung |
|---|---|---|
| `k` | `5` | Anzahl Chunks als Kontext pro Frage |
| `chunk_size` | `800` | Zeichen pro Chunk beim Zerlegen importierter Texte |
| `disable_streaming` | `false` | `true` = Antworten werden nicht Wort-für-Wort gestreamt, sondern komplett gepuffert zurückgegeben |
| `redact_pii` | `false` | E-Mails/Telefonnummern/IBANs/Kartennummern vor dem Einbetten aus dem Text entfernen |
| `enable_draft_replies` | `false` | Button „Antwortentwurf erstellen" in der Quellenansicht bei E-Mails freischalten (schlägt nie automatisch etwas vor, das versendet wird — reiner Vorschlag zur Prüfung) |
| `draft_chat_profile` | leer | Backend für Antwortentwürfe; leer = wie `chat_profile` |
| `enable_chat_history` | `false` | Sidebar-Button „Verlauf" freischalten — Verlauf liegt **nur im Browser** (`localStorage`), nie auf dem Server, siehe README.md "Security / privacy considerations" |
| `lang` | `de` | UI-Sprache (`de`/`en`/`fr`/`it`), auch über Einstellungen → Allgemein → Sprache & Oberfläche änderbar, siehe „Übersetzungen (i18n) erweitern" |

### Ranking (`ranking`)

Steuert, wie die drei Signale zur Trefferbewertung gewichtet werden:
`final_score = vector_weight·Kosinus-Ähnlichkeit + keyword_weight·Stichwort-Überlappung + recency_weight·Aktualität`.

| Feld | Standard |
|---|---|
| `vector_weight` | `0.7` |
| `keyword_weight` | `0.2` |
| `recency_weight` | `0.1` |
| `recency_half_life_days` | `180` (nach so vielen Tagen hat der Aktualitäts-Bonus sich halbiert) |
| `candidate_limit` | `80` (wie viele Kandidaten vor der Neubewertung geladen werden) |
| `max_hits_per_source` | `0` (= aus) — wie viele der K Treffer-Plätze eine einzelne Quelle maximal belegen darf (Diversitäts-Schutz: ein einziges langes Dokument kann sonst alle Kontext-Plätze füllen) |

Die Keyword-Suche ist eine echte Vereinigung („hybrid union"): Chunks, die
per BM25 stark auf die Frage passen, aber nicht unter den besten
Vektor-Kandidaten sind (z. B. exakte Fehlercodes, Artikelnummern, Namen),
werden zusätzlich nachgeladen und regulär mitbewertet — Zugriffsbeschränkungen
(`source_access`, Presets) gelten dabei unverändert. Trifft eine Frage
dieselbe Quelle an mehreren, weit auseinanderliegenden Stellen, enthält
der Kontextblock der Quelle alle Fundstellen-Fenster, getrennt durch eine
`[…]`-Markierung (relevant, wenn `context_chunks_before/after` gesetzt sind).

**Chunking:** Beim Zerlegen importierter Texte bleiben Absätze und
Markdown-Tabellen möglichst zusammen (bei zu großen Tabellen wird die
Kopfzeile in jedem Teil wiederholt); lange Absätze werden bevorzugt an
Satzenden getrennt. Chunks unterhalb einer Markdown-Überschrift (`##` …)
erhalten eine `[Abschnitt: …]`-Kontextzeile, damit Suche und Modell wissen,
zu welchem Kapitel ein Textstück gehört — in der Quellen-Volltextansicht
wird diese Zeile wieder ausgeblendet. Beim erneuten Import einer Quelle
werden nur noch die tatsächlich geänderten Chunks neu eingebettet
(unveränderte übernehmen ihr gespeichertes Embedding), und das Einbetten
passiert **vor** dem Löschen der alten Fassung — ein Ausfall des
Embedding-Backends kann eine Quelle nicht mehr leeren.

### Import (`import`)

| Feld | Standard | Bedeutung |
|---|---|---|
| `markitdown_bin` | `markitdown` | Pfad/Name der markitdown-Programmdatei |
| `max_file_mb` | `25` | maximale Dateigröße beim Datei-Import; bestimmt für Voice-Mode zugleich das Audio-Größenlimit (siehe unten) |
| `originals_dir` | `r3-originals` | Ablageort für Original-Uploads, falls beim Hochladen „Original behalten" gewählt wurde |
| `tesseract_bin` | `tesseract` | Pfad/Name der tesseract-Programmdatei (OCR für Chat-/Agent-Bild-Uploads sowie Bild-Uploads/-Anhänge allgemein, siehe unten) |
| `tesseract_lang` | `deu+eng` | tesseract-Sprachcode(s), mit `+` kombinierbar |
| `whisper_bin` | `whisper-cli` | Pfad/Name des Whisper-kompatiblen CLI-Programms (whisper.cpp-Argumente `-m/-f/-l/-otxt/-of`) |
| `whisper_model` | leer | Pfad zur lokalen Whisper-Modelldatei, z. B. `models/ggml-small.bin` — wird nicht von R3 heruntergeladen |
| `whisper_language` | `de` | Sprachcode für Whisper; leer = automatische Erkennung, sofern das Binary dies unterstützt |
| `whisper_timeout_seconds` | `0` (= 120s) | maximale Laufzeit eines einzelnen Whisper-Prozesses, max. 600 |
| `whisper_threads` | `0` (= Whisper-Standard, 4) | CPU-Threads pro Whisper-Prozess (`--threads`) |
| `whisper_beam_size` | `0` (= Whisper-Standard, 5) | Whisper-Beam-Size (`--beam-size`) — `1` (Greedy-Decoding) ist der übliche Tempo-Trade-off für kurze Sprachaufnahmen |
| `whisper_flash_attn` | `false` | aktiviert Whisper.cpps schnelleren Flash-Attention-Kernel (`--flash-attn`) |
| `whisper_vad` / `whisper_vad_model` | `false` / leer | Sprachaktivitätserkennung (`--vad --vad-model <pfad>`) — überspringt stille Abschnitte vor der Transkription; beide Felder müssen zusammen gesetzt werden, sonst wird die Einstellung beim Speichern abgelehnt |
| `whisper_max_concurrent` | `0` (= 2) | wie viele Whisper-/FFmpeg-Transkriptionen server-weit gleichzeitig laufen dürfen (Diversitäts-/Überlastungsschutz, siehe unten) |

`allow_shell_exec` (Standard `false`, oberste Ebene der Einstellungen) muss
zusätzlich aktiviert sein, sonst werden weder `markitdown` noch
`tesseract`/`ffmpeg`/Whisper je aufgerufen — Office-/PDF-Import schlägt
dann fehl, und ist `upload.image_mode` auf `"ocr"` (Standard), werden
hochgeladene Bilder in Chat/Agent gar nicht gelesen.

**Voice-Mode (lokale Spracheingabe, `/api/voice/transcribe`).** Jede
Transkription normalisiert das hochgeladene Audio zunächst über das
konfigurierte `ffmpeg`-Binary (16 kHz, mono, PCM) und läuft dann durch das
konfigurierte Whisper-CLI — beides ausschließlich lokal, nichts verlässt
den Server, und weder das Audio noch das Transkript werden in der
Wissensbasis oder im Chat-Verlauf gespeichert. `whisper_max_concurrent`
begrenzt, wie viele solcher (rechenintensiven, das Modell jeweils komplett
neu ladenden) Prozesse gleichzeitig laufen dürfen — unabhängig davon greift
`api.guest_voice_rate_limit_per_minute` (siehe „Zugriffskontrolle" unten)
als zusätzliches, pro-Aufrufer-Limit für anonyme Anfragen.

Bilddateien (`.png/.jpg/.jpeg/.gif/.bmp/.tif/.tiff/.webp`) sind auch beim
direkten Datei-Upload und beim Ordner-Import unterstützt: sie laufen — wie
Bild-Anhänge aus E-Mails schon immer — durch tesseract-OCR
(`allow_shell_exec` vorausgesetzt; ein Ordner-Import überspringt Bilder
still, solange OCR deaktiviert ist, statt je Foto einen Fehler zu melden).
Importierte Webseiten (`web_page`) lassen sich außerdem über den
„Neu laden"-Knopf der Quellen-Übersicht einzeln neu abrufen, wie bisher
schon SharePoint-Dateien/-Seiten/-Links.

### Speicher (`storage`) — **nur beim ersten Start / per Neustart änderbar**

```json
"storage": { "backend": "tinysql", "mode": "hybrid", "path": "r3-data", "max_memory_mb": 256 }
```

- `backend`: `tinysql` (Standard, eingebettete Pure-Go-SQL-Engine) oder
  `sqlite` (modernc.org/sqlite, ebenfalls ohne CGO).
- `mode`: nur für `tinysql` relevant — `memory`/`wal`/`disk`/`index`/`hybrid`
  (Details in `docs/VECTOR_DB.md`). Für `sqlite` wirkungslos.
- `path`: Ordner (bei disk/index/hybrid) oder Datei (bei memory/wal bzw. bei
  `sqlite` immer eine einzelne `.db`-Datei).
- `max_memory_mb`: Cache-Obergrenze für die tinySQL-Modi `index`/`hybrid`.

Dieser Block wird **nicht** über die Weboberfläche angeboten, weil ein
Wechsel bedeutet, einen anders aufgebauten Speicher zu öffnen statt einen
laufenden Wert zu ändern — siehe Abschnitt 6 für den unterstützten Weg, ihn
trotzdem zu wechseln, ohne Daten zu verlieren.

### Zugriffskontrolle

- `admin_password_env` (Standard `R3_ADMIN_PASSWORD`): Name der
  Umgebungsvariable mit dem Passwort für den **einfachen** Admin-Login-Gate
  (nur UI-Sichtbarkeit, siehe README.md "Admin access" — **kein** echter
  Zugriffsschutz auf API-Ebene, solange LDAP nicht aktiv ist).
- `ldap` (`enabled`, `url`, `base_dn`, `domain_prefix`,
  `required_group_dn`): echte Active-Directory-Anmeldung mit
  serverseitiger Sitzungsprüfung — die eigentliche Zugriffskontrolle. Ohne
  gesetzte `required_group_dn` erhält jeder erfolgreich authentifizierte
  AD-Account Admin-Zugriff (wird als Warnung geloggt). Erst aktivieren
  (`enabled: true`), wenn `url`/`base_dn`/`required_group_dn` zur
  Zieldomäne passen.
- `local_auth` (`enabled`, `min_password_length`, `bcrypt_cost`): **lokale
  Benutzerkonten** als Alternative und Ergänzung zu LDAP — z. B. für
  externe Partner oder Testkonten ohne AD-Konto, oder für Deployments ganz
  ohne AD. Die Konten selbst werden im Admin-Tab **„Benutzer"** angelegt
  und verwaltet (Benutzername, Anzeigename, E-Mail, Abteilung,
  Admin-Flag, Deaktivieren, Passwort-Reset) und liegen in einer eigenen
  Datei im konfigurierten Speicher-Backend (`storage.backend`: tinySQL
  oder SQLite; Pfad über `storage.users_path`, Standard
  `r3-users-tinysql` bzw. `r3-users.db`) — nie in der `settings.json`.
  Passwörter werden mit **bcrypt** gehasht (Salt im Hash enthalten;
  `bcrypt_cost` Standard 12, `min_password_length` Standard 12). Beim
  Login wird zuerst gegen lokale Konten geprüft, dann gegen LDAP — ein
  lokales Konto mit falschem Passwort fällt bewusst **nicht** auf LDAP
  zurück. Wichtig: Sobald lokale Anmeldung ODER LDAP aktiv ist, sind die
  Admin-Bereiche serverseitig wirklich geschützt (nicht mehr nur
  UI-versteckt). Praktischer Erst-Einrichtungsweg ohne LDAP: bei noch
  deaktivierter lokaler Anmeldung im „Benutzer"-Tab das erste Admin-Konto
  anlegen, dann `local_auth.enabled` einschalten.
- `api` (`require_api_key`): schaltet die externe JSON-API
  (`/api/ask`, `/api/search`) auf Pflicht-API-Key um — Verwaltung der Keys
  selbst läuft über eigene Endpunkte/die Einstellungen-Seite, nicht über
  dieses Feld direkt (siehe `docs/API.md`).
- `api.guest_ask_rate_limit_per_minute` / `api.guest_voice_rate_limit_per_minute`
  (Standard `0` = kein Limit): begrenzen, wie oft ein anonymer Aufrufer
  (keine gültige Anmeldung) pro Minute `/api/ask` bzw.
  `/api/voice/transcribe` aufrufen darf — eingeloggte Nutzer sind jeweils
  ausgenommen. Eine Sprachaufnahme startet einen externen ffmpeg-/
  Whisper-Prozess und ist damit teurer als ein einzelner `/api/ask`-Aufruf,
  daher das eigene, unabhängige Limit statt eines gemeinsamen Budgets.

### Externe Quellen (Connectoren) — jede einzeln optional

Jeder Connector ist standardmäßig `enabled: false` und tut ohne Aktivierung
nichts.

**Mehrere Verbindungen pro Connector-Typ.** SharePoint, Outlook/Exchange
(Graph), IMAP, Teams, Confluence, Jira und Freshservice erlauben beliebig
viele **benannte** Verbindungen gleichzeitig, statt nur einer festen — z. B.
zwei Exchange-Postfächer oder zwei SharePoint-Sites nebeneinander. In der
Settings-UI erscheint dafür pro Connector-Typ eine Karten-Liste mit einem
„+ Verbindung hinzufügen"-Button; jede Karte hat oben rechts ein
einheitliches „⋮"-Menü mit **Testen** (wie bisher, gegen die noch nicht
gespeicherten Werte der Karte), **Duplizieren** (kopiert die Karte
vollständig inkl. aller Zugangsdaten, setzt aber `aktiv` auf aus — bewusst
kein versehentlich doppelt laufender Connector), **Exportieren**
(lädt die Karte als `.json` herunter; direkte Secret-Felder wie Passwort/
Client-Secret/Token werden dabei geleert, die zugehörigen `_env`-Felder
mit dem Namen der Umgebungsvariable bleiben erhalten), **Importieren**
(liest eine zuvor exportierte `.json` wieder ein und ersetzt damit alle
Felder dieser einen Karte — eine Datei eines anderen Connector-Typs wird
abgelehnt) und **Entfernen** (wie bisher). Der `name` einer Verbindung ist rein intern (Zuordnung im
Import-Tab, im Scheduler-Log, beim Speichern) und wird der Gegenstelle nie
mitgeteilt — muss aber innerhalb seines Connector-Typs eindeutig sein.
Eine bestehende `settings.json` mit dem alten Einzelverbindungs-Format wird
beim ersten Start automatisch zu einer Ein-Elemente-Liste migriert
(`name: "default"`), ohne manuellen Eingriff.

*Import-Tab, aktueller Stand:* Die Vorschau-/Import-Buttons im Import-Tab
wählen ohne explizite Angabe automatisch die einzige konfigurierte
Verbindung eines Typs — bei genau einer Verbindung (der Normalfall)
funktioniert „Import jetzt" also unverändert. Bei **mehreren** Verbindungen
desselben Typs braucht der Request zusätzlich ein `connection`-Feld mit dem
Verbindungsnamen (sonst Fehler „connection name required"); eine
Verbindungsauswahl direkt im Import-Tab (statt nur über die API) ist noch
nicht gebaut.

Jede Verbindung hat zusätzlich zu ihren eigenen Zugangsdaten drei Felder,
die den globalen Import-Standard (siehe „Drosselung & Limits" oben)
für genau diese eine Verbindung überschreiben, 0/leer jeweils = globaler
Standard:

- **Zyklus** — bei IMAP `poll_interval_seconds` (Sekunden), bei allen
  anderen sechs `sync_interval_minutes` (Minuten): lässt den
  Hintergrund-Scheduler diese Verbindung automatisch laufen, siehe
  „Hintergrund-Scheduler" unten.
- **Limit** (`max_items_per_run`) — Obergrenze an Elementen pro Lauf nur
  für diese Verbindung.
- **Timeout** (`timeout_seconds`) — wie lange ein einzelner Lauf dieser
  Verbindung höchstens dauern darf, bevor er abgebrochen wird.

**Jede Karte unten hat einen "Verbindung testen"-Button** (ebenso die
LLM-Backend- und LDAP-Karten oben) — testet die aktuell im Formular
eingetragenen, noch **nicht gespeicherten** Werte gegen den echten Dienst,
bevor man überhaupt auf "Einstellungen speichern" klickt. Jeder Test nutzt
denselben Code-Pfad wie der echte Import/Versand (z. B. dieselbe
Preview-Funktion, die auch der Import-Tab beim Laden der Auswahlliste
aufruft) statt einer separaten, leichtgewichtigeren Prüfung, die grün
anzeigen könnte, obwohl der echte Connector scheitert. Zwei Karten
brauchen dafür einmalige Zusatzangaben, die nirgends gespeichert werden:
LDAP fragt nach einem Testbenutzer/-passwort (führt einen echten
Bind-Versuch durch, genau wie ein normaler Login), SMTP nach einem
Test-Empfänger (verschickt sofort eine echte Testmail dorthin — anders als
die "An mich senden"-Funktion im Chat, die immer nur an die eigene
angemeldete AD-Adresse verschickt, siehe `smtpConfig.From` in
`settings.go`, darf dieses Admin-only-Debug-Feld ein beliebiges Ziel sein).
Ein fehlgeschlagener Test ist keine Fehlermeldung im technischen Sinn,
sondern eine normale Antwort mit rot markiertem Ergebnistext — praktisch,
um z. B. ein Jira-API-Token oder eine SharePoint-App-Registrierung zu
prüfen, ohne vorher extra in den Import-Tab wechseln zu müssen.

**SharePoint** (`sharepoint`) — Dokumente aus einer SharePoint-Bibliothek:
`tenant_id`, `client_id`, `client_secret`/`client_secret_env`, `site_url`,
optional `sync_interval_minutes` für automatischen Delta-Sync (siehe
„Hintergrund-Scheduler" unten). Importiert zusätzlich zur Dokumentbibliothek
auch die **Site Pages** einer Site (`.aspx`-Seiten, als extrahierter
Textinhalt, nicht die rohe Seiten-Markup) sowie einzelne Dateien über eine
eingefügte **ShareLink**-URL — praktisch für den häufigen Fall „mir wurde
ein Link geteilt", ohne erst durch die ganze Bibliothek browsen zu müssen.
Zusätzlich eine **Discover**-Aktion, die die Site-/Bibliotheks-/
Ordnerstruktur rekursiv abläuft und als Vorschau anzeigt, bevor überhaupt
etwas importiert wird — hilfreich, um eine große SharePoint-Site erst
gezielt einzugrenzen, statt Pfade zu raten.
Benötigt eine Azure-AD-App-Registrierung mit Client-Credentials-Flow (kein
Login/Passwort/MFA eines Menschen — die App meldet sich als eigenständige
Maschinen-Identität an).

*Berechtigung: `Sites.Selected`, nicht `Sites.Read.All`.* `Sites.Read.All`
gibt der App Lesezugriff auf **den gesamten Tenant** — bei Rubix mit vielen
Marken, Standorten und historisch zusammengeführten Gesellschaften (siehe
unten) ein unnötig großer Radius, der auch persönliche OneDrives oder
HR-interne Bereiche anderer Landesgesellschaften einschließen würde.
`Sites.Selected` erzwingt stattdessen pro Site eine bewusste,
IT-durchgeführte Freigabe: Ohne diesen expliziten Schritt sieht die App
gar keine Site. Vorgehen: App-Registrierung anlegen → `Sites.Selected`
*Application*-Berechtigung beantragen → Global-Admin-Consent einmalig
bestätigen → für jede gewünschte Site einen expliziten Freigabe-Befehl
ausführen (Microsoft Graph `sites/{site-id}/permissions`, macht die IT,
nicht R3).

*Rollout stufenweise, nicht alles auf einmal.* Erst mit dem jeweiligen
Fachbereich abgestimmte Bereiche freigeben, nicht den ganzen Tenant in
einem Schritt. Betriebsrats-/Personalunterlagen (Gehaltsdaten, Personalakten)
vorher explizit mit Betriebsrat/HR/Datenschutz klären, statt sie
"aus Versehen" mitzuindexieren, nur weil sie im selben Drive wie
unkritische Inhalte liegen — bei Rubix potenziell **mehrere** historisch
getrennte Betriebsräte/HR-Prozesse (siehe unten). Ebenso wichtig: R3 selbst
wertet SharePoint-Berechtigungen beim Antworten nicht aus (`settings.
source_access` filtert nach Abteilung, nicht nach SharePoint-ACLs) — wer in
SharePoint ein Dokument nicht öffnen dürfte, sollte es also erst gar nicht
über diesen Connector einspielen lassen. Das gilt **genauso** für die
optionale Live-Suche (`live_search_enabled`, `search_sharepoint`-Tool in
Chat/Agent): R3 meldet sich bei Microsoft Graph immer app-only an (keine
Anmeldung eines einzelnen Menschen), und Graphs automatisches
Security-Trimming für `/search/query` greift nur bei einer delegierten
(eingeloggter Mensch) Anfrage — nicht bei app-only. Die Live-Suche zeigt
also jedem R3-Nutzer dieselben Treffer, begrenzt nur durch die
`Sites.Selected`-Freigabe der App, nicht durch die persönliche
SharePoint-Berechtigung der fragenden Person. Live-Suche ist damit **keine**
sicherere Alternative zum Bulk-Import — dieselbe Sichtbarkeit, nur live
statt vorab eingelesen.

*Betriebliche Realitäten:* Access-Tokens laufen nach ca. 1 Stunde ab (R3
erneuert das automatisch, siehe `graph.go`), Client-Secrets laufen nach der
von der IT festgelegten Frist ab (empfohlen 12–24 Monate) — das läuft ohne
Vorwarnung ins Leere, wenn niemand die Rotation überwacht; das ist aktuell
kein automatisierter R3-Alarm, sondern ein Punkt für die
Betriebs-Checkliste. Microsoft Graph drosselt bei zu vielen Anfragen
(HTTP 429) — größere Importläufe brauchen dafür Retry-mit-Backoff (siehe
`docs/DEPLOYMENT.md`/Code-Kommentare in `graph.go`, falls das noch nicht
umgesetzt ist). Ordner- und Seitennamen wie im SharePoint tatsächlich
angelegt (auch mit Tippfehlern) werden unverändert übernommen, R3
"korrigiert" sie nicht.

*Warum Rubix hier besonders ist:* Rubix GmbH ist aus mehreren, historisch
eigenständigen Unternehmen zusammengewachsen (1998 als ZITEC
Industrietechnik GmbH gegründet, 2019 Umbenennung in Rubix GmbH plus
Verschmelzung mit Brammer GmbH und Kistenpfennig AG, später u. a. Schäfer
Technik). Praktisch bedeutet das: gewachsene, uneinheitliche
Ordner-/Benennungskonventionen sind die Regel, nicht die Ausnahme (der
Code darf sie nicht "korrigieren" wollen); es kann weitere Sprach-/
Länder-Sites neben `intranet_de` geben, falls europäische Kolleg:innen
eingebunden werden sollen (frühzeitig mit der IT klären, bevor man sich
auf eine Single-Site-Architektur festlegt); und viel Content dürfte aus
Produktdaten/Datenblättern/Lieferantendokumenten bestehen (technischer
Großhandel) — PDFs mit Tabellen/technischen Zeichnungen sind hier
wahrscheinlicher als bei einem reinen Text-Intranet.

*Kommunikation an Kolleg:innen* — Vorlage für die Fachbereichs-Info, statt
Panik vor "die KI liest alles" zu erzeugen:

> „Unser neues Such-/Assistenzsystem liest automatisch Dokumente aus dem
> Intranet (SharePoint) ein — zum Beispiel Anleitungen, Formulare oder
> interne Infos aus bestimmten Bereichen. Ihr müsst dafür nichts tun: Ihr
> ladet eure Dateien wie gewohnt in SharePoint hoch, das System holt sie
> sich automatisch von dort ab, meist einmal pro Nacht oder alle paar
> Stunden.
>
> Das System sieht dabei nur die Bereiche, die vorher gemeinsam mit der IT
> und den jeweiligen Fachabteilungen freigegeben wurden — nicht automatisch
> alles, was im Intranet liegt. Es greift dafür nicht mit einem persönlichen
> Login zu, sondern über eine technische Berechtigung, die extra dafür
> eingerichtet wurde.
>
> Wenn ein Dokument im System noch nicht auftaucht: Das ist meist eine
> Frage der Zeit (der nächste automatische Abgleich holt es nach) oder der
> Bereich ist schlicht noch nicht für die Indexierung freigegeben."

Dabei nicht mit Echtzeit-Versprechen kommunizieren, wenn der Sync
tatsächlich nur alle paar Stunden läuft, und eine feste Anlaufstelle für
"Dokument fehlt/ist falsch" benennen (IT oder Projektteam), statt das
unstrukturiert zu lassen.

**Outlook/Exchange Online** (`exchange_graph`) — Postfach über Microsoft
Graph: `tenant_id`, `client_id`, `client_secret`/`client_secret_env`,
`mailbox`, `folder` (Standard `"inbox"`). Braucht die Graph-Berechtigung
`Mail.Read` (App-Berechtigung, Admin-Consent). Anhänge werden automatisch
mitimportiert (`source_kind` `outlook_attachment`) — nur Dateianhänge
(`fileAttachment`), keine weitergeleiteten Nachrichten oder
OneDrive/SharePoint-Links als Anhang.

*Interaktiver Postfach-Zugriff im Mail-Tab* (`interactive_enabled` +
`allowed_users`/`allowed_groups`) — unabhängig vom obigen Import: lässt ausgewählte,
angemeldete Benutzer im Mail-Tab ihr **eigenes** Postfach (ihre eigene
AD-E-Mail-Adresse, nicht das oben konfigurierte `mailbox`) durchsuchen,
eine Nachricht auswählen (Datum/Uhrzeit/Absender/Betreff, dann Volltext
ohne Tab-Wechsel), einen Antwortentwurf mit optionalen zusätzlichen
Hinweisen erzeugen lassen und diesen direkt im eigenen Outlook-
Entwürfe-Ordner ablegen — nutzt dieselbe App-Registrierung/Graph-
Berechtigung wie oben (App-only-Auth kann jedes freigegebene Postfach
adressieren, keine zusätzliche Nutzer-Anmeldung bei Microsoft nötig).
`allowed_users` ist eine Freigabeliste (E-Mail oder AD-Login, eine Zeile
je Zeile im Formular) — leer bedeutet niemand, nicht jeder; ein nicht
gelisteter Benutzer nutzt weiterhin den bisherigen Copy-and-Paste-Ablauf
unverändert. `allowed_groups` (seit Phase 4, eine AD-Gruppen-DN je Zeile)
erweitert dieselbe Freigabe um AD-Gruppen — Mitglied einer hier gelisteten
Gruppe ODER in `allowed_users` gelistet reicht, beide leer bedeutet
weiterhin niemand. Das Ablegen im eigenen Postfach braucht zusätzlich
`enable_draft_replies` (dieselbe Schreibfreigabe wie für automatische
Entwürfe oben) — Lesen/Entwurf-Anzeigen funktioniert auch ohne. R3
versendet dabei, wie überall, nichts selbst.

**IMAP** (`imap`) — On-Prem Exchange oder beliebiges IMAP-Postfach ohne
Azure-AD-App: `host`, `port` (Standard `993`), `username`,
`password`/`password_env`, `mailbox` (Standard `"INBOX"`), `use_tls`
(Standard `true`). `last_uid` wird automatisch von R3 verwaltet (nicht von
Hand setzen) — merkt sich den zuletzt importierten Stand für inkrementelle
„Import jetzt"-Klicks. Anhänge werden aus der MIME-Struktur jeder Nachricht
extrahiert und einzeln importiert (`source_kind` `imap_attachment`).
Zusätzlich `drafts_mailbox` (Standard `"Drafts"`): der IMAP-Ordner, in den
der Mail-Tab geprüfte Entwürfe ablegt („In Postfach-Entwürfe speichern") —
das einzige Schreiben, das R3 je gegen das Postfach ausführt; versendet
wird weiterhin ausschließlich von einem Menschen im Mail-Programm.

**Microsoft Teams** (`teams`) — Kanal-Nachrichten samt ihrer Thread-
Antworten (ein Beitrag plus alle seine Antworten wird als EIN Dokument
importiert, siehe unten): `tenant_id`, `client_id`,
`client_secret`/`client_secret_env`, `team_id`, `channel_id` (beide IDs
stehen in der Teams-Web-URL des Kanals). Braucht
`ChannelMessage.Read.All` (tenant-weite App-Berechtigung — Zugriff auf
diese App-Registrierung entsprechend einschränken). Wie viele Antworten
je Thread maximal übernommen werden, steuert
`import.teams_max_replies_per_thread` (Standard 200, siehe „Import
(`import`)" oben).

**Confluence** (`confluence`) — Seiten aus einem Confluence-Cloud-Space:
`base_url` (z. B. `https://firma.atlassian.net/wiki`), `email`,
`api_token`/`api_token_env`, `space_key`. Token erzeugen unter
Atlassian-Konto → Sicherheit → API-Tokens.

**Jira** (`jira`) — Issues aus einem Jira-Cloud-Projekt: `base_url` (z. B.
`https://firma.atlassian.net`, **ohne** `/wiki`-Suffix), `email`,
`api_token`/`api_token_env`, `project_key`. Derselbe Atlassian-API-Token
wie für Confluence funktioniert, sofern der Account beide Bereiche lesen
darf.

**Freshservice** (`freshservice`) — Tickets aus einer Freshservice-Instanz:
`base_url` (z. B. `https://firma.freshservice.com`), `api_key`/
`api_key_env` (als HTTP-Basic-Benutzername mit dem literalen Passwort `X`,
Freshservice-Konvention). Einziger Connector, der zusätzlich zum manuellen
Import auch **unbeaufsichtigt** laufen kann:
`sync_interval_minutes` (Standard `0` = kein Auto-Sync) lässt
`scheduler.go` alle X Minuten selbstständig neue/geänderte Tickets
importieren — siehe „Hintergrund-Scheduler" weiter unten für die
Lauf-Historie dazu.

**Generische Webseiten** — kein Settings-Block nötig; URLs werden direkt im
Import-Tab eingegeben, optional mit rekursivem Crawlen gleicher-Domain-Links
bis zu einer konfigurierten Tiefe statt nur der eingefügten URL selbst.
Siehe `webimport.go`'s Kommentar zu SSRF-Aspekten, bevor dieses Feature für
nicht-vertrauenswürdige Eingaben geöffnet wird.

**RSS/Atom-Feeds** — ebenfalls kein Settings-Block; eine Feed-URL im
Import-Tab einfügen, jeder Feed-Eintrag wird als eigene zitierbare Quelle
importiert. Teilt sich mit der Webseiten-Karte den „🧪 Dry-Run"-Schalter und
den eigenen „Probelauf (testen)"-Knopf, da beide Karten keine feste
Connector-Konfiguration haben, gegen die sich „Verbindung testen" prüfen
ließe.

**Shop** (`shop`) — ebenfalls **kein Import**, sondern ein Live-Werkzeug
(`search_shop_items`): das Chat-Modell kann live Produktdaten aus dem
Rubix-Onlineshop nachschlagen (`base_url`, Standard `https://de.rubix.com`),
Zugangsdaten `username`/`password`/`password_env`. Login läuft normalerweise
über einen Bearer-Token (`POST /rest-api/v1/tokens`), aber der Shop
antwortet auf einen Login-Versuch manchmal mit HTTP 200 ohne JSON-Token-Feld
und stattdessen einem `Set-Cookie` — R3 erkennt das automatisch und weicht
dann auf eine **Cookie-Sitzung** aus (wie ein normaler Browser-Login), samt
einmaligem automatischem Re-Login bei einer 401 mitten in einer Anfrage.
Für diesen Fall gibt es keine gesonderte Einstellung — R3 behandelt beide
Login-Arten transparent gleich.

**MSSQL** (`mssql`) — **kein Import**, sondern ein Live-Werkzeug, das das
Chat-Modell bei Bedarf selbst aufruft (Function/Tool-Calling):
`host`, `port` (Standard `1433`), `database`, `username`,
`password`/`password_env`, `trust_server_certificate` (Standard `true`,
für On-Prem-Server mit selbstsigniertem Zertifikat), `max_rows` (Standard
`200`), `timeout_seconds` (Standard `10`). **Wichtig:** Der Datenbank-Login
sollte selbst nur Lesezugriff (SELECT) haben — die eingebaute
SELECT-only-Prüfung ist zusätzlicher Schutz, kein Ersatz für eine echte
read-only-Datenbank-Rolle. Braucht außerdem ein Chat-Modell mit
Tool-Calling-Unterstützung; ohne das bleibt die Einstellung wirkungslos.
Zusätzlich `mask_columns`: eine Liste von Spaltennamen (Groß-/
Kleinschreibung egal), die in **jedem** Abfrageergebnis — egal ob freie
Abfrage oder SQL-Abfrage-Vorlage — durch `•••` ersetzt werden, bevor das
Chat-Modell sie sieht, z. B. `["email", "phone", "iban"]` für eine
Kundentabelle, die für Zeilenzahlen/Aggregate nutzbar bleiben soll, ohne
dass das Modell personenbezogene Spalten im Klartext bekommt.

Statt freier SELECTs sind **SQL-Abfrage-Vorlagen** der empfohlene Weg: fest
vorgegebene, benannte Abfragen mit typisierten Parametern, jede als eigenes
Werkzeug fürs Modell. Angelegt werden sie in Einstellungen → MSSQL über
einen **strukturierten Editor** (Werkzeugname, Beschreibung fürs Modell,
SQL mit `{name}`-Parametern, je Parameter Typ/Pflicht/Beschreibung/Beispiel,
und ein Rückgabe-Hinweis) — kein Handschreiben von JSON mehr; ein
ausklappbares „Als JSON bearbeiten" bleibt für Fortgeschrittene. Wichtig:
Beschreibung, Parameter-Beschreibung/-Beispiel und Rückgabe-Hinweis liest
das Modell **wortwörtlich** — daran entscheidet sich, ob es die Abfrage
richtig und im passenden Moment nutzt; den SQL-Text selbst sieht es nie.
`{name}` ist reine Autoren-Syntax: intern wird daraus vor der Ausführung
ein echter, über den Treiber gebundener `@name`-Parameter (nie Text-Einbau
ins SQL) — ein vor dieser Vereinheitlichung angelegtes `@name`-Template
wird beim nächsten Start automatisch nach `{name}` migriert, ohne
Handarbeit.
Die **HTTP-Abfrage-Vorlagen** (Live-Abfragen gegen REST-APIs, Zugangsdaten
von einem konfigurierten Connector) funktionieren genauso, mit demselben
Editor und derselben `{name}`-Syntax — inklusive `{platzhalter}`-Variablen in der URL selbst, nicht nur im
SQL-Text. Für ein generisches SAP-se16-Gateway
(`https://logistic.rubix-intern.de/se16/{mandant}/{tabelle}/{nummer}`) legt
man dazu **eine** Vorlage mit URL
`https://logistic.rubix-intern.de/se16/ZITEC/{table}/{id}` und zwei
Parametern an: `table` (Typ `string`, Pflicht) und `id` (Typ `string`,
Pflicht). Für `table` genau die erlaubten SAP-Tabellen als **„Erlaubte
Werte"** eintragen (`likp, vbak, kna1, mbew, mara, …`, kommagetrennt) —
das wird dem Modell als feste Auswahl angeboten (JSON-Schema-`enum`) **und**
zusätzlich serverseitig vor jeder Ausführung geprüft: ein Wert außerhalb
der Liste löst einen Fehler aus, bevor überhaupt eine Anfrage rausgeht,
unabhängig davon, was das Modell tatsächlich schickt (dieselbe
"nicht nur aufs Modell verlassen"-Haltung wie bei MSSQLs SELECT-only-Prüfung).
Was `id` in der jeweiligen Tabelle bedeutet, unterscheidet sich je Tabelle
und lässt sich nicht als feste Werteliste ausdrücken — das gehört in die
**Beschreibung** des `id`-Parameters, wortwörtlich vom Modell gelesen, z. B.
„bei `likp`/`vbak` die Belegnummer (`VBELN`), bei `mara`/`mbew` die
Materialnummer (`MATNR`), bei `kna1` die Kundennummer (`KUNNR`)". Ein
Parameter mit einer solchen Werteliste (egal ob HTTP- oder SQL-Vorlage)
akzeptiert **nur** die dort gelisteten Werte — leer lassen (Standard)
erlaubt weiterhin jeden Wert des gewählten Typs, wie bisher.

Jeder `{name}`-Platzhalter in der URL wird passend zu seiner Position
kodiert, nicht pauschal gleich: vor dem ersten `?` (Pfad) escaped wie ein
URL-Pfadsegment, ab dem ersten `?` (Query-String) escaped wie ein
Query-Parameter — sonst könnte ein Modell-Wert wie `4711&extra=1` in
`?id={id}` einen zusätzlichen, vom Admin nie vorgesehenen Query-Parameter
einschleusen. Ein `?` im gelieferten Wert selbst wird immer kodiert, kann
also nie eine neue Query-String-Grenze erzeugen.

**Zugriffsbeschränkung je Live-Werkzeug** (`access_control`, seit Phase 4)
— MSSQL, Shop und jeder generische REST-Connector (Abschnitt „Generischer
REST-Connector" unten) haben zusätzlich zur bestehenden Registriert-/
Admin-Zugriffsstufe ein eigenes `access_control` mit `allowed_users`
(E-Mail oder Login-Name) und `allowed_groups` (AD-Gruppen-DN, geprüft gegen
das `memberOf` der anmeldenden Person). **Leer bedeutet hier ausdrücklich
keine zusätzliche Einschränkung** — anders als bei den weiter unten
beschriebenen `allowed_users`/`allowed_groups` des interaktiven
Postfach-Zugriffs (dort bedeutet leer „niemand"): diese drei Werkzeuge
kannten vor Phase 4 gar keine Identitäts-Einschränkung, ein Upgrade ohne
Anfassen dieses Felds darf das Verhalten also nicht ändern. Ein REST-
Connector hat sein eigenes `access_control` in seiner eigenen Karte
(Einstellungen → Generische REST-Connectoren) — es gilt dann für **jede**
HTTP-Abfrage-Vorlage, die diesen Connector per `auth_source` nutzt.

### R3s eigenen Quellcode importieren (nur Admin, `selfsource.go`)

Im Import-Tab gibt es eine eigene Karte „🐕 R3-Quellcode importieren"
(`POST /api/import/self-source`, admin-gated): sie lädt R3s eigenen
Go-/JS-/CSS-/HTML-/Markdown-Quellcode in die eigene Wissensbasis, damit R3
auch Fragen zu seiner eigenen Implementierung beantworten kann — mit
Zitat auf die jeweilige Datei (`source_kind` `r3_source`). Läuft immer nur
als bewusste, einmalige Admin-Aktion — nie automatisch beim Start oder
über den Scheduler.

Der Import läuft ausschließlich im Arbeitsverzeichnis des Server-Prozesses
selbst und schließt dabei — als Sicherheitsnetz, nicht nur zur Auswahl
sinnvoller Dateien — konsequent aus: `.git`, `external/` (Referenz-/
Scratch-Projekte, kein Teil von R3, siehe `AGENTS.md`), `docs/` (lokal
sensible Arbeitsnotizen), jedes `r3-data*`-/`verify*`-Speicher-Verzeichnis,
sowie **jede** `settings*.json`-/`.env`-/`credentials*`-/`.key`-/
`.pem`-Datei unabhängig von deren Dateiendung — `settings.json` enthält in
einem echten Betrieb echte Zugangsdaten (LDAP-Bind-Passwort, API-Keys),
die niemals in einer zitierfähigen Wissensbasis landen dürfen.

### Hintergrund-Scheduler

Jede der 7 Mehrfachverbindungs-Connectoren kann zusätzlich zum manuellen
Import (Button-Klick im Import-Tab) unbeaufsichtigt auf einem Intervall
laufen, jeweils **pro einzelner Verbindung** über deren eigenes Zyklus-Feld
(Standard überall 0 = kein Auto-Sync, exakt das bisherige Verhalten für
eine migrierte oder neu angelegte Verbindung, die das Feld nie anfasst).
`scheduler.go` baut die Job-Liste bei jedem Tick (alle 30 Sekunden) neu aus
den aktuellen Einstellungen auf — eine neu angelegte oder umbenannte
Verbindung wird ohne Neustart wirksam:

- **Freshservice, Confluence, Jira, Teams** —
  `sync_interval_minutes`: holt einmal pro Intervall alle Elemente per
  Vorschau und ingestiert sie (dieselbe „alles vorschlagen, alles
  auswählen, importieren"-Logik wie ein manueller Import mit
  Alles-auswählen). Teams folgt dabei der Graph-Paginierung (auch mehr als
  50 Beiträge pro Lauf) und importiert je Beitrag den **gesamten Thread**:
  die Antworten landen mit im selben Dokument (in Gesprächsreihenfolge,
  gelöschte Antworten ausgenommen), und das Dokumentdatum entspricht der
  jüngsten Aktivität im Thread — nicht dem Eröffnungsbeitrag.
- **Outlook/Exchange (Graph)** — `sync_interval_minutes`: läuft
  **inkrementell** über eine je Verbindung gemerkte Wasserstandsmarke
  (`last_synced_received`, das `receivedDateTime` der zuletzt bearbeiteten
  Nachricht). Der allererste Lauf startet wie bisher mit den neuesten
  N (Vorschau-Limit) Nachrichten und setzt die Marke; jeder weitere Lauf
  listet ab der Marke **vorwärts** (älteste zuerst) — ein Rückstand, der
  größer als das Limit eines Laufs ist, wird über mehrere Läufe
  abgearbeitet statt verloren zu gehen, und ein Nachrichten-Schub zwischen
  zwei Läufen kann nicht mehr „durchrutschen". Die Marke ist wie
  `delta_link`/`last_uid` server-verwaltet; auf `""` zurücksetzen erzwingt
  einen frischen Start mit den neuesten N.
- **IMAP** — `poll_interval_seconds`: holt alles oberhalb der je
  Verbindung gemerkten `last_uid` (derselbe inkrementelle Abruf wie
  „Import jetzt") und schreibt die neue Hochwassermarke zurück.
- **SharePoint** — `sync_interval_minutes`: führt den Graph-Delta-Sync
  aus (geänderte Dateien ingestieren, entfernte löschen) und schreibt
  den je Verbindung eigenen `delta_link`-Cursor fort — wie der
  „Delta-Sync"-Button im Import-Tab.

**Scheduler-Dashboard** — eigener Tab „Jobs" im Hauptmenü (zwischen
Prompts und Einstellungen, admin-only wie diese beiden): eine Zeile je
aktivierter Verbindung — auch für Verbindungen ganz ohne Intervall (dann
als „nur manuell" markiert). Je Zeile sichtbar: das konfigurierte
Intervall, der Live-Status (läuft gerade seit wann / pausiert / wartet
mit Uhrzeit des nächsten Laufs) und der letzte Lauf (✓/✗, Zeitpunkt,
Dauer, Auslöser `auto` oder `manuell`, Details als Tooltip). Aktualisiert
sich alle 10 Sekunden von selbst, solange der Jobs-Tab der aktive Tab ist.
Drei Aktionen pro Job
(`/api/scheduler/run`, `/api/scheduler/cancel`, `/api/scheduler/pause`,
alle admin-geschützt):

- **Jetzt ausführen** — startet den vollständigen Sync dieser Verbindung
  sofort (ad-hoc), unabhängig von Intervall und Pause; ein bereits
  laufender Job wird nicht doppelt gestartet.
- **Abbrechen** — stoppt einen gerade laufenden Job über dessen Kontext.
  Greift zwischen zwei Elementen, nie mitten in einem einzelnen Request
  (ein hängender Verbindungsaufbau läuft bis zu seinem eigenen
  Netzwerk-Timeout weiter); bereits importierte Elemente bleiben
  erhalten, der Lauf erscheint als „abgebrochen" im Verlauf.
- **Pausieren/Fortsetzen** — setzt nur den automatischen Rhythmus aus,
  ohne das konfigurierte Intervall zu vergessen (anders als das Intervall
  auf 0 zu stellen). Persistiert in `paused` je Verbindung, wird aber vom
  normalen „Einstellungen speichern" **nicht** überschrieben (Server-
  verwaltet wie `last_uid`/`delta_link`) — ein veralteter, noch offener
  Settings-Tab kann eine Pause also nicht versehentlich aufheben.
  Manuelle Imports im Import-Tab und „Jetzt ausführen" bleiben trotz
  Pause möglich. Einen bereits laufenden Job stoppt Pausieren nicht —
  dafür ist „Abbrechen" da; beides zusammen hält einen Connector
  vollständig an.

Darunter der Verlauf: die letzten Läufe aller Jobs (automatisch wie
manuell gestartete), je unter dem Job-Namen
`<connector>-sync:<verbindungsname>`, z. B. `imap-sync:support-postfach` —
so lässt sich bei mehreren Verbindungen desselben Typs erkennen, welche
gerade lief. Nur im Arbeitsspeicher (max. 50 Einträge), geht bei einem
Serverneustart verloren.

`scheduler.go` ist dafür ein kleiner, abhängigkeitsfreier Job-Runner
(eine simple Ticker-Schleife, kein `robfig/cron`, da hier nie mehr als
„alle N Minuten" gebraucht wird). Ein Job, der bei Fälligkeit des
nächsten Ticks noch läuft, wird einfach übersprungen statt in die
Warteschlange gestellt — eine langsame Instanz kann also keine
überlappenden Importe derselben Quelle anhäufen.

Die letzten 50 Läufe aller Jobs werden im Arbeitsspeicher gehalten (`GET
/api/scheduler/history`, admin-geschützt) und im Panel „Letzte
Auto-Sync-Läufe" (Freshservice-Karte, zeigt alle Jobs) angezeigt —
Job-Name, Zeitstempel, Dauer, Erfolg/Fehler und eine kurze Detailzeile
(Element-/Chunk-/Skip-Zahlen oder die Fehlermeldung). Das ist reine
Betriebs-Sichtbarkeit, kein Audit-Trail: geht bei einem Serverneustart
verloren, absichtlich, genau wie der Chat-Verlauf im Browser.

### Zitat-Sichtbarkeit je Quelltyp (`source_visibility`)

Legt je `source_kind` fest, ob dessen Treffer dem Menschen als anklickbare
Quelle/Zitat in der Antwort angezeigt werden — ein nicht aufgeführter Typ
bleibt sichtbar (Standardverhalten, kein Migrationsschritt nötig):

```json
"source_visibility": {
  "pst_email": false,
  "pst_attachment": false
}
```

Auf `false` gesetzte Typen tragen weiterhin ganz normal zur Beantwortung
bei (die Rangfolge/Auswahl der Chunks ist davon unberührt) — nur die
Quellenangabe selbst (Name, Link, Chip in der Oberfläche) wird nach der
Antwortgenerierung unterdrückt. Gedacht für Fälle wie einen
PST-Postfachimport, der eine Antwort fundieren, aber aus
Datenschutzgründen nicht als benannte Quelle auftauchen soll. Über
Einstellungen → „Zitate (Sichtbarkeit je Quelltyp)" editierbar (ein
Eintrag pro Zeile, Format `source_kind = true/false`).

Unabhängig davon zeigt R3 ohnehin nur Quellen als Zitat an, deren
„[Qn]"-Marker tatsächlich im Antworttext des Modells auftaucht — ein von
der Vektorsuche gefundener, aber vom Modell nie verwendeter Kandidat wird
nie zitiert, auch ohne diese Einstellung.

### Zitate (`url_mappings`)

Ordnet lokale Pfad-Präfixe (wie sie in `source_id` stehen) einer
Web-erreichbaren URL zu, damit Quellenangaben im Chat direkt anklickbar
werden:

```json
"url_mappings": [
  { "prefix": "C:\\Freigaben\\Dokumente\\", "url_prefix": "https://intranet.rubix.com/docs/" }
]
```

Die Liste wird von oben nach unten geprüft, der erste passende Präfix
gewinnt.

## Bedienung der Web-Oberfläche

- **Chat** — Fragen stellen, Antworten mit Quellenangaben; Klick auf eine
  Quelle öffnet den Volltext (und, falls beim Import „Original behalten"
  gewählt wurde, einen Download-Link für die Originaldatei). „Neu
  generieren" unter einer Antwort verwirft sie (und alles Spätere in diesem
  Gespräch) und stellt die Frage erneut. Ist `enable_chat_history` aktiv,
  merkt der Browser vergangene Gespräche im „Verlauf"-Button. Trifft eine
  Antwort eine importierte E-Mail (oder deren Anhang), wandern automatisch
  auch die zugehörigen Geschwister — Anhänge bzw. die Ursprungs-Mail — mit
  in den Kontext und erscheinen als eigene Quellenangaben. Über die
  Büroklammer im Eingabefeld lässt sich zusätzlich ein Bild/Foto/Scan
  anhängen (max. 4 Anhänge, je max. `upload.max_attachment_mb`, Standard
  8 MB) — für genau diese eine Frage, nirgends gespeichert (nicht im
  „Verlauf", nicht im Browser). Die Frage selbst ist auf
  `upload.max_prompt_chars` Zeichen begrenzt (Standard 20000) — eine
  längere Frage wird mit Fehlermeldung abgelehnt statt gekürzt. Ist unter
  Einstellungen → Chat → „Anhänge & Eingabelänge (Chat/Agent)" →
  „Bild-Uploads verarbeiten über" `"Vision-Modell"` gewählt, wird die
  **komplette Frage** an das dort festgelegte Vision-Backend geschickt —
  unabhängig vom Profil-Dropdown
  neben dem Eingabefeld; ist nichts erreichbar (kein Backend gewählt,
  oder eine Anmeldung fehlt für Azure), wird das Bild ignoriert und ganz
  normal ohne Bildinhalt weitergefragt. Steht dort `"markitdown/OCR"`
  (Standard), wird das Bild immer per `tesseract`-OCR in Text umgewandelt
  (braucht `allow_shell_exec` + installiertes `tesseract`). Der
  Hinweistext am Büroklammer-Symbol zeigt an, welcher der beiden Fälle
  gerade greift. Auf dem Vision-Pfad wird ein zu großes Bild vor dem
  Versand automatisch verkleinert (längste Kante standardmäßig max.
  1600px, neu als JPEG kodiert; unter Einstellungen → LLM-Backends &
  Routing per `vision_max_dim`/`vision_jpeg_quality` auf 800–1600px bzw.
  Qualität 50–95 einstellbar) — Foto-Uploads vom Handy sind oft
  3000-4000px breit, was das Modell nicht besser lesen lässt, nur Anfrage
  und (bei manchen Anbietern) Kosten unnötig aufbläht; hängen mehrere
  Bilder an derselben Frage, bekommt jedes eine
  Dateinamen-Bildunterschrift, damit das Modell in seiner Antwort
  zwischen ihnen unterscheiden kann. Der OCR-Pfad bekommt weiterhin die
  unveränderten Originalbilder, da `tesseract` von höherer Auflösung
  profitiert. Dieselbe Bild-Upload-Funktion gibt es identisch, egal welche
  Chat-Stufe (Instant/Standard/Agent) gerade gewählt ist.
- **Mail** (sichtbar, wenn `enable_draft_replies` aktiv ist) — zwei Modi:
  eine erhaltene E-Mail einfügen und einen Antwortentwurf bekommen, oder
  Empfänger/Thema/Stichpunkte beschreiben und eine neue E-Mail formulieren
  lassen — beides gestützt auf die Wissensbasis, inklusive
  Betreff-Vorschlag. Der Entwurf entsteht **agentisch**: das Modell darf
  von sich aus in mehreren Runden weitere Chunks nachladen
  (`search_knowledge_base`), einen Volltext öffnen (`get_source_content`),
  den Shop durchsuchen oder eine freigegebene SQL-/HTTP-Abfrage-Vorlage
  ausführen, bevor es schreibt — dieselben Werkzeuge wie in der
  Chat-Agent-Stufe, gesteuert über dasselbe Draft-Preset. Betreff und Text sind direkt
  editierbar; über „📎 Datei anhängen" lassen sich zusätzlich beliebige
  Dateien (Scans, Fotos, PDFs, …) an den Entwurf hängen (max. 5, je max.
  15 MB) — anders als beim Bild-Upload in Chat/Agent bleiben diese
  erhalten, bis der Entwurf neu generiert oder der Anhang von Hand entfernt
  wird, und landen unverändert in allen drei folgenden Aktionen. Danach
  kopieren, als `.eml` herunterladen (öffnet in Outlook & Co. — mit Anhang
  über einen kurzen Serveraufruf, der dieselben MIME-Bytes wie die beiden
  folgenden Aktionen erzeugt), per „An mich senden" oder — nur für Admins,
  IMAP konfiguriert — per „In Postfach-Entwürfe speichern" im
  Entwürfe-Ordner des Postfachs ablegen. R3 versendet nie selbst; geprüft
  und verschickt wird immer von einem Menschen im Mail-Programm.
- **Mein Konto** (Sidebar-Button unten, sichtbar für jede angemeldete
  Person, sobald LDAP-Login aktiv ist) — zeigt die aus AD übernommenen
  Felder (Name, Abteilung, Titel, Standort) sowie einen Abschnitt
  „Persönlicher Kontext (optional)": frei editierbare eigene Angaben
  (bevorzugter Name, Position, Abteilung, Kontaktdaten, bevorzugte
  Kommunikationsweise, Signatur, typische Formulierungen, Hinweise für die
  KI). Diese Angaben fließen in Antworten/Mail-Entwürfe nur ein, wenn
  **zusätzlich** das Häkchen „Für Personalisierung verwenden" gesetzt ist
  **und** serverweit `personalize_answers` aktiv ist (Einstellungen →
  Zugriffskontrolle → „Antworten personalisieren") — sonst bleiben die
  Angaben gespeichert, aber wirkungslos. Ein eigener „Speichern"-Button
  sichert nur diesen Abschnitt, unabhängig von der Sprachauswahl im
  selben Modal (getrennte Felder in derselben Nutzer-Tabelle, damit sich
  beide Sicherungen nicht gegenseitig überschreiben). Eine hinterlegte
  Signatur ersetzt automatisch die Standard-Signatur unter
  Mail-Entwürfen.
- **Agent-Stufe** (kein eigener Tab mehr — seit „Agent-Tier in Chat
  zusammengeführt" ist Agent die dritte Option im Stufen-Dropdown oben im
  Chat-Fenster: **Instant** · **Standard** · **Agent**; `tab-agent.html`
  gibt es nicht mehr). Instant beantwortet ohne jede Wissensbasis-Suche,
  Standard ist der bisherige einmalige Retrieval-Durchlauf, Agent darf
  Werkzeuge mehrfach und iterativ einsetzen (bis zu „Max. Werkzeug-Runden",
  Standard 6, Einstellungen → Agent): Wissensbasis gezielt durchsuchen
  (`search_knowledge_base`), Volltexte nachladen (`get_source_content`),
  Quellen inventarisieren (`list_sources`) — alles mit denselben
  Abteilungs-Zugriffsregeln wie Standard-Chat. Ein Wechsel der Stufe
  mitten im Gespräch ist möglich. Bei breiten Fragen mit mehreren
  unabhängigen Teilen kann die Agent-Stufe die Aufgabe zerlegen und
  mehrere **Unter-Agenten parallel** bearbeiten lassen
  (`delegate_subtasks`) und deren Ergebnisse zusammenführen — standardmäßig
  an, abschaltbar über „Unter-Agenten deaktivieren" (Einstellungen →
  Agent); Unter-Agenten können selbst keine weiteren starten. Über jeder
  Antwort zeigt eine **Arbeitsschritte-Zeitleiste** live mit, was der Agent
  gerade tut (jeder Werkzeug-Aufruf, jeder Unter-Agent) — und der Button
  **„Demo starten"**, sichtbar bei leerem Gesprächsverlauf sobald die
  Agent-Stufe gewählt ist, führt eine Beispielaufgabe aus, die genau diese
  Mehrschritt-Orchestrierung sichtbar macht. Die
  Parallel-Orchestrierung ist unter Einstellungen → Agent steuerbar: max.
  Teilaufgaben je Delegation (Standard 4), Werkzeug-Runden je Unter-Agent
  (Standard 4) und — als Drossel gegen zu viel gleichzeitige Last auf das
  Chat-Backend — max. gleichzeitige Unter-Agenten (Standard 4); alle mit
  Obergrenze 8. Sind Antwortentwürfe aktiviert, kann er zusätzlich
  Mail-Entwürfe formulieren und (nur Admins, IMAP konfiguriert) im
  Entwürfe-Ordner des Postfachs ablegen — versendet wird weiterhin nie.
  Jeder Werkzeug-Aufruf landet im Protokoll „Letzte Agent-Werkzeug-Aufrufe"
  (Einstellungen → Agent), Unter-Agenten-Aufrufe dabei ihrem Unter-Agenten
  zugeordnet und eingerückt; „Leeren" setzt das Protokoll zurück.
  Code-Ausführung ist vorbereitet, aber in diesem Build ohne Sandbox
  deaktiviert (siehe `docs/AGENT_PLAN.md`). Mit `agent.allow_web_fetch`
  darf die Agent-Stufe zusätzlich eine einzelne öffentliche Webseite
  abrufen (`fetch_url`, nie ins Wissensbasis-Archiv, nur in den
  Antwort-Kontext, folgt keinen Links); `import.allow_internal_fetch`
  (Standard aus, Einstellungen → Import) hebt dabei gezielt die sonst
  geltende Sperre gegen interne/private Adressen auf, für den seltenen
  Fall einer bewusst internen Ziel-URL. Mit zusätzlich
  `agent.allow_web_research` darf sie stattdessen einen eigenen
  Rechercheauftrag (`web_research`) starten, der von einer Startseite aus
  selbstständig mehrere Seiten verfolgt, bis das Ziel gefunden ist oder das
  eigene Runden-/Zeitbudget aufgebraucht ist, und nur eine kurze
  Zusammenfassung mit Quell-URLs zurückgibt (nie die besuchten Rohseiten).
  Mit `agent.allow_web_search` steht zusätzlich `web_search` bereit: eine
  echte Stichwortsuche über die Tavily-API (eigener API-Key unter
  Einstellungen → Agent nötig), die eine kurze Trefferliste (Titel,
  Kurzauszug, URL) liefert — der Such-/Entdeckungsschritt, den
  `fetch_url`/`web_research` von sich aus nicht haben, weil beide bereits
  eine bekannte URL brauchen. Mit `agent.allow_azure_bing_search` steht
  stattdessen (oder zusätzlich) `azure_bing_search` bereit: eine Frage wird
  über Azure OpenAI's „Grounding with Bing Search" (Responses API) direkt
  mit einer fertigen, quellenbelegten Antwort beantwortet, statt einer
  Trefferliste — nutzt das ohnehin unter „LLM-Backend: Azure OpenAI"
  hinterlegte Deployment mit, kein separater Schlüssel nötig, aber ein
  GPT-4-Klasse-Deployment erforderlich (das Werkzeug wird sonst gar nicht
  erst angeboten).
- **Import** — Dateien hochladen, PST-Postfachexporte importieren (mit
  Ordnerauswahl), sowie jeder aktivierte Connector aus Abschnitt 4 als
  eigene Karte, inklusive Jira. Bei PST-, IMAP- und Outlook/Exchange-Import
  werden E-Mail-Anhänge automatisch mit extrahiert und als eigene,
  zitierbare Quelle importiert (Office/PDF-Anhänge benötigen wie bei
  Datei-Uploads `allow_shell_exec` + `markitdown`). Für PST-Dateien, die
  für einen Browser-Upload zu groß oder
  das Netzwerk zu langsam sind, gibt es zusätzlich zum Datei-Upload ein
  Feld „Pfad auf dem Server" — die Datei wird dann direkt vom
  Server-Dateisystem gelesen (z.B. nach Kopie per Netzlaufwerk oder `scp`),
  ganz ohne HTTP-Upload. Siehe „Große PST-Postfächer importieren" unter
  Fehlerbehebung für die Details und eine Checkliste.
  Oben auf der Import-Seite schaltet „🧪 Dry-Run" jeden Import darunter in
  einen Simulationsmodus: Extraktion, Zerlegung in Chunks und der
  Duplikat-Check (unverändert vs. neu/geändert) laufen normal, aber es wird
  nichts eingebettet oder in die Wissensbasis geschrieben — auch
  Fortschrittsmarker wie SharePoints Delta-Link oder IMAPs zuletzt
  importierte UID werden dabei nicht aktualisiert. Das Ergebnis zeigt genau,
  was passiert wäre, bevor man den echten Import startet. Die Karten
  „Webseite" und „RSS/Atom" haben zusätzlich einen eigenen Knopf
  „Probelauf (testen)", der immer einen Dry-Run auslöst — das Gegenstück
  zum „Verbindung testen" der anderen Connectoren für diese beiden reinen
  URL-Quellen.
- **Drosselung & Limits** (Einstellungen → Import) — schützt R3 und die
  angebundenen Systeme davor, dass ein Import in einem Rutsch riesige
  Mengen zieht oder die Gegenstelle mit Anfragen überflutet, und gilt für
  **alle** Connectoren: „Max. Elemente pro Import-Lauf" (Standard 500)
  deckelt jeden Lauf; „Pause zwischen Anfragen (ms)" drosselt aktiv
  zusätzlich zum reaktiven Zurückfahren nach einem 429; „Vorschau-Größe"
  steuert, wie viele Kandidaten die Vorschau-Listen holen. Resumierbare
  Quellen (IMAP über die UID, SharePoint-Delta über den Delta-Link) machen
  beim nächsten Lauf automatisch dort weiter, wo das Limit gegriffen hat —
  ein großer Rückstand wird in Häppchen abgearbeitet, es geht nichts
  verloren.
- **Quellen** — alle importierten Quellen mit Umfang/Aktualität; einzeln
  oder gesammelt (nach Typ oder Import-Charge) löschbar. Zusätzlich ein
  Filter über Suchbegriff (Name oder Pfad), Typ und Dateiendung — die
  Dropdowns befüllen sich automatisch mit den tatsächlich vorhandenen
  Werten. Sobald mindestens ein Filter aktiv ist, erscheint „Gefilterte
  Quellen löschen" mit Live-Zähler, für z. B. „nur alle PDFs" oder „alles
  zu einem bestimmten Kunden" löschen, ohne gleich einen ganzen Quelltyp zu
  treffen (`/api/sources/delete-by-filter`, `deleteSourcesByFilter` in
  `store.go`). Der Bestätigungsdialog zeigt dabei die **serverseitig**
  gezählten Treffer (Dry-Run-Vorabfrage), nicht den möglicherweise
  veralteten Stand der angezeigten Tabelle.
- **Chunks** — jeder gespeicherte Chunk einzeln, mit Volltextsuche, Filtern
  und einem live berechneten Aktualitäts-Score (derselben Formel, die auch
  die Rangfolge im Chat bestimmt).
- **Prompts** — der globale System-Prompt (`prompts/index.md`) sowie
  beliebig viele Fach-„Skills" (`prompts/skill_*.md`), die nur bei
  passenden Stichwörtern in die Frage eingemischt werden. Änderungen wirken
  sofort, ohne Neustart.
- **Einstellungen** — alles aus Abschnitt 4, als Formular.

Die Tabs Import/Quellen/Chunks/Prompts/Einstellungen sind standardmäßig nur
nach Admin-Login sichtbar (Button unten links in der Seitenleiste). Ohne
aktives LDAP ist das **nur eine UI-Anzeige-Konvenienz**, kein echter
Zugriffsschutz auf API-Ebene — siehe README.md "Admin access" und Abschnitt
9 unten.

Admins haben zusätzlich zwei Extras, beide an den Admin-Status gebunden
(`ldap.admin_users` bzw. AD-Gruppe), nicht an einen fest verdrahteten
Benutzer:

- **Voller Zugriff ohne Abteilungsfilter** — die abteilungsbezogene
  Zugriffsbeschränkung (`source_access`) greift für Admins nicht: Chat/
  Agent-Retrieval, das direkte Öffnen einer Quelle/eines Chunks und der
  Entwurf aus einer Quelle sehen für Admins alles, passend zum Chunks-Tab,
  der Admins ohnehin jeden Chunk ungefiltert zeigt.
- **Debug-Modus** — Chat, Agent und Mail blenden für Admins ein
  ausklappbares Debug-Panel ein: welche Chunks mit welchen Teil-Scores
  geladen wurden, die exakt an das Modell geschickten Nachrichten, jeder
  Werkzeug-Aufruf mit Dauer, die Roh-Antwort sowie Profil/Preset/
  Abteilungscode und Gesamtdauer der Anfrage.

**Verbindungstests** (Einstellungen, je Connector „Verbindung testen")
zeigen für die HTTP-basierten Connectoren auf Klick den vollständigen,
Secret-bereinigten Request und Response jedes HTTP-Aufrufs — mit Text- und
Hex-Ansicht, um auch eine verstümmelte/leere Antwort zu erkennen. Jede
Änderung an den Einstellungen landet zudem in einer „Änderungshistorie"
(wer, wann, welches Feld alt→neu) — Passwörter/Tokens werden dabei nur als
„geändert" ohne Wert protokolliert.

**Admin-Benachrichtigungen** — kurze Erfolgs-/Fehlermeldungen (z. B. „Import
X fertig") erscheinen als Toast oben rechts, live per Server-Sent Events an
jedes offene Admin-Browserfenster gepusht, statt wie früher alle 8 Sekunden
abgefragt zu werden — spart unnötige Anfragen im Zugriffslog. Nur im
Arbeitsspeicher, geht bei einem Neustart verloren wie der Scheduler-Verlauf.

## Aktualisieren / Warten

### Quellcode aktualisieren

```bash
go mod tidy   # löst exakte (indirekte) Abhängigkeiten neu auf
go build .
```

`go mod tidy` ist besonders nach dem Hinzufügen/Ändern eines Connectors
wichtig (neue Abhängigkeiten wie `go-imap`, `go-mssqldb`, … müssen in
`go.sum` landen).

### Backup

Zu sichern sind: das Speicherverzeichnis/-datei (`storage.path` aus
`settings.json`), `settings.json` selbst, und — falls genutzt —
`import.originals_dir` (aufbewahrte Original-Uploads) sowie `prompts_dir`
(System-Prompt/Skills). `make snapshot` packt das gesamte Projektverzeichnis
(ohne den Speicherordner) als ZIP/TAR — für den Speicherordner selbst separat
sichern (er kann groß werden).

### Speicher-Backend wechseln (tinySQL ↔ SQLite)

Ohne Daten zu verlieren oder neu einzubetten:

```bash
make migrate MIGRATE_FROM_BACKEND=tinysql MIGRATE_FROM_PATH=r3-data \
             STORAGE_BACKEND=sqlite APP_STORAGE=r3-data.db
```

Das kopiert jeden Chunk **mit vorhandenem Embedding** vom alten in das neue
Backend und beendet sich danach, ohne den Server zu starten. Anschließend
R3 mit den neuen `-storage-backend`/`-storage-path`-Werten (bzw. der
entsprechenden `storage`-Sektion in `settings.json`) neu starten.

### Neustart-Verhalten

- Sitzungen (LDAP-Login) überstehen einen Neustart jetzt: Sowohl der
  HMAC-Signierschlüssel der Sitzungs-Cookies als auch der Sitzungsspeicher
  selbst werden auf Platte geschrieben (`initSessionPersistence`/
  `loadOrCreateSessionSecret` in `session.go`), statt bei jedem
  Prozessstart neu im Arbeitsspeicher erzeugt zu werden — ein geplanter
  Neustart (Deploy, Konfigurationsänderung) meldet angemeldete Nutzer also
  nicht mehr zwangsweise ab.
- IMAP-„last_uid" und ähnliche Fortschritts-Marker in `settings.json`
  bleiben erhalten — inkrementelle Importe setzen nach einem Neustart genau
  dort fort, wo sie waren.

## R3 erweitern

Dieser Abschnitt richtet sich an Entwickler:innen, die R3 um eigene
Fähigkeiten erweitern.

### Architektur in Kürze

- **`main.go`** — Startpunkt, CLI-Flags, eingebettete Web-Assets
  (`//go:embed`).
- **`settings.go`** — die gesamte Konfigurationsstruktur (`appSettings`) plus
  Laden/Speichern.
- **`handlers.go`** — praktisch alle HTTP-Endpunkte; `registerRoutes`
  listet sie vollständig auf.
- **`store.go`/`rank.go`/`vectorstore*.go`** — der eigentliche RAG-Kern:
  Chunking, Embedding, hybrides Ranking, austauschbares Speicher-Backend
  hinter dem `vectorStore`-Interface.
- **`ingest.go`** — der eine gemeinsame Schreibpfad
  (`ingestDocument`/`replaceSourceChunks`), durch den *jeder* Import läuft —
  Datei-Upload, PST, jeder Connector. Wer einen neuen Import-Weg baut,
  funnelt am Ende hier durch, statt eine eigene Speicherlogik zu erfinden.
- **Ein Connector = eine Datei:** `sharepoint.go`, `graphmail.go`,
  `imapmail.go`, `teams.go`, `confluence.go`, `webimport.go` folgen alle dem
  gleichen Muster — Vorschau laden → Auswahl → Import mit
  Fortschritts-Streaming (NDJSON). `graph.go` bündelt die für
  SharePoint/Exchange/Teams gemeinsame Microsoft-Graph-Authentifizierung.
- **`llm.go`** — der OpenAI/Azure-kompatible LLM-Client, inklusive
  generischem Tool-/Function-Calling (`chatWithTools`) — die Grundlage für
  `mssql.go`s Live-Datenbank-Werkzeug; ein zusätzliches Werkzeug registriert
  sich genauso (`toolDef` + `toolExecutor`), ohne `chatWithTools` selbst
  anzufassen.
- **`web/`** — Frontend: `index.html` (Struktur), `app.js` (Logik, ein
  einziges Skript ohne Build-Schritt), `style.css`, `i18n.js`
  (Sprach-Mechanismus, siehe unten).

### Einen neuen Import-Connector hinzufügen

Am einfachsten anhand eines bestehenden, ähnlichen Connectors vorgehen (z. B.
`confluence.go` für eine weitere REST-API mit Basic-Auth, oder
`graphmail.go` für einen weiteren Microsoft-Graph-Endpunkt):

1. Neue `*Config`-Struktur in `settings.go` (Vorbild: `confluenceConfig`),
   `enabled: false` als Standard.
2. Neue Datei mit `previewX`/`importX`-Funktionen, die am Ende
   `ingestDocument(...)` aus `ingest.go` aufrufen — das übernimmt Chunking,
   Embedding, Deduplizierung (per Inhalts-Hash) und das Ersetzen alter
   Chunks bei erneutem Import automatisch.
3. Zwei Endpunkte in `handlers.go` (`.../preview`, dann der eigentliche
   Import mit NDJSON-Fortschritts-Stream) plus Eintrag in `registerRoutes`.
4. Karte im Import-Tab (`web/index.html`) + zugehöriges JavaScript
   (`web/app.js`) — auch hier lohnt sich eine bestehende Karte als Vorlage.
5. Test-Datei nach Vorbild von `confluence_test.go`/`teams_test.go`: ein
   `httptest`-Server steht für die echte externe API ein, sodass die
   Logik ohne echten Zugang zur echten Quelle geprüft werden kann.

### Übersetzungen (i18n) erweitern

`web/i18n.js` enthält den Mechanismus (`t()`, `applyI18n()`) plus ein
`MESSAGES`-Wörterbuch, aktuell mit vier Sprachen: `de`, `en`, `fr`, `it`
(`supportedUILangs` in `settings.go`). Admin wählt die Oberflächensprache
unter Einstellungen → Allgemein → Sprache & Oberfläche (`lang`, serverweit
statt pro Browser) — wirkt sofort nach „Einstellungen speichern", kein
Neustart nötig. Übersetzt sind aktuell
alle Endnutzer-Flächen (Sidebar, Chat/Agent/Mail, Hilfe, Verlauf, Mein
Konto, Theme-/Schriftgrößen-Switcher, Login) — die admin-lastigen
Bereiche (Import-Connector-Karten, Verbindungstest-Buttons,
Chunks-Filterleiste, Scheduler/Jobs) haben noch hart deutschen Text
ohne `data-i18n`-Tagging. Eine weitere Sprache hinzuzufügen bedeutet:
`MESSAGES.<sprache>` mit denselben Schlüsseln befüllen und eine
`<option>` im Sprache-Dropdown ergänzen — an den Seiten, die
`t()`/`data-i18n*` schon nutzen, ändert sich nichts.

### Die JSON-API von außen nutzen

Siehe [`docs/API.md`](docs/API.md) (Authentifizierung, Endpunkte,
Statuscodes) und die maschinenlesbare Spezifikation unter
`GET /api/openapi.json` (bzw. `docs/openapi.json`), die sich direkt in
Swagger Editor/Postman/Codegeneratoren laden lässt. Zum schnellen
Durchklicken direkt im Browser gibt es außerdem `GET /api/docs` — eine
eingebettete, interaktive OpenAPI/Swagger-Oberfläche mit „Ausprobieren"-
Funktion (echte Testanfragen gegen die laufende Instanz), ganz ohne
externe Abhängigkeit (kein CDN, kein vendorisiertes swagger-ui-Bundle).

### Tests & Qualitätssicherung

```bash
make check   # fmt + vet + test
```

Neue Backend-Logik bekommt möglichst eine Testdatei nach bestehendem Vorbild
(`*_test.go` neben der jeweiligen Implementierung). Für alles, was einen
echten externen Dienst braucht (echtes AD, echter Azure-Tenant, echte
Mailbox, echter SQL-Server, …), gilt: mit einem `httptest`-Server die
Protokoll-/Verarbeitungslogik prüfen, und im PR/Commit klar benennen, was
zusätzlich gegen die echte Gegenstelle verifiziert werden muss (siehe
README.md "What still needs real-environment verification").

## Fehlerbehebung

- **`go build` scheitert beim Parsen von `go.mod`** — die installierte
  Go-Version ist älter als in `go.mod` gefordert (≥1.23 bestätigt
  funktionsfähig). Aktuelle Go-Version installieren.
- **Chat-/Embedding-Anfragen hängen oder schlagen mit Verbindungsfehler
  fehl** — der konfigurierte lokale Server (`-url`, Standard
  `http://localhost:1234`) läuft nicht oder hat das angeforderte Modell
  nicht geladen. Mit `-verbose` (bzw. `make dev`) starten, um jeden
  Embed-/Chat-Call mit Provider/Modell/Basis-URL im Log zu sehen.
- **Office-/PDF-Dateien lassen sich nicht importieren** — `allow_shell_exec`
  ist aus, oder `markitdown` ist nicht installiert/nicht im `PATH`.
  `pip install markitdown` ausführen und `allow_shell_exec` in den
  Einstellungen aktivieren.
- **Eine Einstellung scheint nicht zu wirken** — die meisten Felder wirken
  sofort ab dem nächsten Request; der `storage`-Block ist die Ausnahme und
  braucht einen Neustart (siehe oben).
- **LDAP-Login schlägt immer fehl** — `url`/`base_dn`/`domain_prefix` gegen
  das tatsächliche AD prüfen; mit `-verbose` werden fehlgeschlagene Versuche
  geloggt (inkl. Grund, ohne das Passwort selbst zu loggen). Nach zu vielen
  Fehlversuchen von derselben IP greift zusätzlich eine 5-Minuten-Sperre
  (siehe `ratelimit.go`) — das ist dann kein Konfigurationsfehler, sondern
  Absicht.
- **Ein Connector-Import bleibt bei "not enabled" stehen** — der
  jeweilige `enabled`-Schalter in `settings.json`/den Einstellungen ist
  noch aus, oder Pflichtfelder (Tenant/Client/Secret/…) fehlen.
- **Fragen zu großen Importmengen dauern lange** — betrifft primär den
  *Import*, nicht die spätere Suche (siehe `docs/VECTOR_DB.md` zur
  Speicher-Architektur); bei tinySQL ggf. den Modus `hybrid` statt `disk`
  prüfen.
- **Große PST-Postfächer importieren (mehrere hundert MB bis mehrere GB)**
  — R3 selbst hat keine feste Obergrenze für die Dateigröße (weder beim
  Upload noch beim Import); trotzdem lohnt sich vor einem großen Import
  diese Checkliste:
  - **Am zuverlässigsten: Server-Pfad statt Browser-Upload.** Die
    PST-Karte im Import-Tab hat neben dem Datei-Upload ein zweites Feld
    für einen Pfad auf dem Server. Datei vorher per Netzlaufwerk, `scp`
    o.ä. auf den Server kopieren, dann dort den vollständigen Pfad
    eintragen — kein HTTP-Upload, keine Browser-/Proxy-Timeouts, keine
    Fortschrittsanzeige-Lücke während des Uploads selbst.
  - **Reverse-Proxy-Limits prüfen**, falls einer vor R3 steht (siehe
    `docs/DEPLOYMENT.md`): nginx' `client_max_body_size` (Standard oft nur
    1 MB) oder ein entsprechendes Limit einer Firewall/eines Load
    Balancers würde einen Browser-Upload schon vor R3 abweisen — betrifft
    nicht den Server-Pfad-Weg, da dort gar keine HTTP-Anfrage mit der
    Datei selbst nötig ist.
  - **Freien Plattenplatz prüfen**: ein hochgeladener PST wird zunächst in
    ein temporäres Verzeichnis kopiert (mind. einmal die Dateigröße frei
    nötig); ein per Server-Pfad angegebener PST wird nicht kopiert. Dazu
    kommt in beiden Fällen das Wachstum des Vektor-Speichers
    (`storage.path`) durch die neu importierten Chunks.
  - **Embedding-Backend-Durchsatz einplanen**: das Einlesen der PST-Datei
    selbst ist günstig (Nachrichten werden gestreamt, nicht komplett in
    den Speicher geladen); der eigentliche Zeitaufwand entsteht durch die
    Embedding-Anfrage pro Chunk an das konfigurierte LLM-Backend
    (lokal oder Azure) — bei mehreren tausend Nachrichten entsprechend
    Geduld bzw. Kapazität auf der Backend-Seite einplanen.
  - Danach am besten mit den Fortschrittsmeldungen während des Imports
    (Ordner/Nachrichtenzahl live im Browser) und der Fehlerliste am Ende
    prüfen, ob alles wie erwartet durchlief.

## Sicherheit & Datenschutz — kurz

- **Kein eingebauter Server-Zugriffsschutz ohne LDAP** — ohne aktives
  `ldap.enabled` sind die Admin-Tabs nur in der Oberfläche versteckt, nicht
  auf API-Ebene geschützt. Für einen produktiven Einsatz: entweder LDAP
  aktivieren oder R3 hinter einen Reverse-Proxy mit eigener
  Authentifizierung stellen bzw. den Netzwerkzugriff einschränken.
- **`redact_pii`** entfernt gängige personenbezogene Muster vor dem
  Einbetten — sinnvoll, wenn ein Postfach-Export breiter zugänglich sein
  wird als das ursprüngliche Postfach selbst.
- **Chat-Verlauf** (falls aktiviert) liegt ausschließlich im Browser der
  jeweiligen Person, nie auf dem Server.
- **API-Keys** werden nur als Hash gespeichert; der Klartext wird genau
  einmal (bei Erstellung) angezeigt.
- **Login-Versuche** sind pro IP rate-limitiert (10 Fehlversuche / 5
  Minuten), schützt aber nicht gegen verteilte Angriffe über viele
  IP-Adressen — dafür weiterhin ein Reverse-Proxy/WAF nötig.
- PST-Dateien und Postfach-Exporte sind sensible Daten — Speicherverzeichnis
  und aufbewahrte Original-Uploads entsprechend absichern wie den
  ursprünglichen Postfachzugriff selbst.

Ausführlicher: `README.md`, Abschnitt "Security / privacy considerations".

## Weiterführende Dokumente

- [`README.md`](README.md) — technische Architektur-Entscheidungen und
  Begründungen (Englisch).
- [`docs/API.md`](docs/API.md) — externe JSON-API im Detail.
- [`docs/openapi.json`](docs/openapi.json) — maschinenlesbare API-Spezifikation.
- `GET /api/docs` — interaktive OpenAPI/Swagger-Oberfläche der laufenden Instanz.
- [`docs/VECTOR_DB.md`](docs/VECTOR_DB.md) — Speicher-/Such-Architektur,
  wann ein Wechsel auf etwas Größeres (pgvector o. ä.) sinnvoll würde.
- [`docs/DEPLOYMENT.md`](docs/DEPLOYMENT.md) — Checkliste für den ersten
  echten Produktionseinsatz.
- [`docs/HARDENING_PLAN.md`](docs/HARDENING_PLAN.md) — geplante nächste
  Sicherheits-/Funktionsschritte.
