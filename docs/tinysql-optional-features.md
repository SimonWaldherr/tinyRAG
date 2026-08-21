# Optionale tinySQL-Funktionen

tinyRAG aktiviert die neuen tinySQL-Funktionen nicht automatisch. Die Schalter
finden sich unter **Einstellungen** und behalten den bisherigen Betrieb als
Standard bei.

- **Datenbank-Audit:** Schreibt ein hash-verkettetes JSONL-Protokoll aller
  tinySQL-Statements. Optional kann ein eigener Dateipfad gesetzt werden.
- **Speicher-Verschlüsselung:** Benötigt `disk`, `index` oder `hybrid` als
  Storage-Modus und `TINYRAG_STORAGE_KEY`. Der Wert muss genau 32 Byte als Hex
  oder Base64 enthalten. Er wird nie in `settings.json` gespeichert. WAL wird
  absichtlich abgelehnt, da tinySQL diesen Modus nicht verschlüsselt.
- **Native Vektorsuche:** Aktiviert `VEC_SEARCH`. tinyRAG holt ein erweitertes
  Kandidatenfenster und wendet danach die bestehenden Rollen- und ACL-Filter an.
  Der Standard `scalar` bleibt für strengste Recall-Anforderungen erhalten.
- **Hybrid-Suche (Vektor + BM25):** Nutzt tinySQLs `HYBRID_SEARCH`, um die
  Kosinus-Trefferliste per Reciprocal-Rank-Fusion mit einem echten BM25-Volltext-
  Pass über die Chunk-Inhalte zu verschmelzen. Findet dadurch exakte Begriffe
  (Fehlercodes, Produkt-IDs, Eigennamen), die eine reine Vektorsuche übersehen
  kann. Genau wie bei `vector` wird danach das erweiterte Kandidatenfenster mit
  den Rollen-/ACL-Filtern eingeschränkt; der an das R³-Ranking übergebene Score
  bleibt eine gewöhnliche Kosinus-Ähnlichkeit, nicht der interne RRF-Wert.
- **Vektor-Index (`flat`/`ivf`/`hnsw`):** Wählt den von `VEC_SEARCH` und
  `HYBRID_SEARCH` verwendeten Suchindex. `flat` (exakter Scan) ist Standard und
  empfohlene Baseline; `ivf`/`hnsw` lohnen sich erst bei größeren Korpora und
  sollten vorher am eigenen Datenbestand benchmarkt werden (tinySQLs eigene
  Benchmarks zeigen, dass `flat`/`ivf` `hnsw` bei kleinen Korpora schlagen
  können). Beim Start wird der gewählte Index (bzw. bei `flat` nur der
  Vektor-Spalten-Cache) einmalig vorgewärmt (`VEC_WARM`), damit die erste
  Anfrage nicht den einmaligen Indexaufbau bezahlt. Bei `scalar` (Standard)
  entfällt das Vorwärmen, da dieser Pfad `VEC_SEARCH` gar nicht verwendet.
- **Geodaten-Import:** Erlaubt Uploads von GeoJSON, KML und OpenStreetMap XML
  über den Open-Data-Tab. Jeder Import erhält R3-Quellmetadaten und wird
  idempotent über den Dateinamen aktualisiert.

Audit und Verschlüsselung werden beim Start eingerichtet. Änderungen an diesen
beiden Optionen benötigen daher einen Neustart.

## tinySQL v0.40.0

tinyRAG nutzt seit diesem Upgrade tinySQL v0.40.0 (zuvor v0.19.1 → v0.39.0 →
v0.40.0). Die von tinyRAG verwendete API blieb bei jedem Schritt vollständig
abwärtskompatibel — es waren keine Codeänderungen zum reinen Kompilieren
nötig. v0.40.0 ist ein reines internes Performance-Release (u. a. ein
Prepared-Statement-Fast-Path für INSERT/UPDATE/DELETE, eine binäre statt
lineare B-Tree-Suche, mehrere Allokationsvermeidungen); keine öffentliche
API oder SQL-Syntax hat sich geändert. Seit v0.39.0 neu genutzt werden:

- `HYBRID_SEARCH` / `RAG_SEARCH` für den neuen `hybrid`-Retrieval-Modus.
- `VEC_WARM` zum Vorwärmen von Vektor-Cache und ANN-Index beim Start.
- Das `index`-Argument von `VEC_SEARCH` (`flat`/`ivf`/`hnsw`), jetzt über die
  Einstellung „Vektor-Index" konfigurierbar statt hart auf `flat` gesetzt.

## Strukturbewusstes Chunking & Ingestion-Härtung

Unabhängig von tinySQL wurde die Ingestion-Pipeline generisch gehärtet, damit
größere, stärker strukturierte Wissensbestände (verschachtelte Referenzseiten
mit nummerierten Abläufen, Tabellen und Glossaren; oder viele kleine
strukturierte Lerninhalte) zuverlässiger verarbeitet werden — ohne dass sich
das Verhalten für bestehende, unstrukturierte Inhalte ändert:

- **Blockbewusstes Chunking:** Listen-, Tabellen- und Code-Block-Zeilen werden
  vor dem Zuschneiden zu atomaren Einheiten gruppiert, sodass ein
  Zeichenbudget-Schnitt nicht mehr mitten in einer Schritt-für-Schritt-Anleitung
  oder einer Tabellenzeile landet. Reiner Fließtext verhält sich unverändert.
  Eine einzelne Zeile/Einheit, die selbst das Budget sprengt, wird jetzt
  zeilen- bzw. wortgrenzenbewusst weiter aufgeteilt statt unbegrenzt
  durchgereicht zu werden.
- **Office-Struktur bleibt erhalten:** Die Text-Extraktion aus
  DOCX/PPTX/XLSX/ODT/ODP/ODS erzeugt jetzt an Absatz-, Zeilen- und
  Zellgrenzen echte Zeilenumbrüche statt alles zu einer einzigen Zeile zu
  verschmelzen.
- **Neue Quellart „structured_item":** Für kleine, in sich abgeschlossene
  Datensätze (Glossareintrag, Tabellenzeile, Kursmodul, Frage-Antwort-Paar),
  automatisch gesetzt beim CSV-/JSON-Zeilenimport.
- **Konfigurierbare Terminologie** (`terminology`, verwaltet über
  `GET`/`POST /api/settings/terminology`): Begriffspaare (z. B. Abkürzung ↔
  ausgeschriebene Form), die die Abfrageerweiterung automatisch in beide
  Richtungen ergänzt. Leer per Standard.
- **Update-Modus für Wiki-/URL-/Text-Import:** `/api/add-wiki`,
  `/api/add-url` und `/api/add-text` akzeptieren jetzt optional ein
  `metadata`-Objekt mit `update_mode` (`skip` Standard / `upsert` /
  `replace`), damit erneut importierte, inzwischen geänderte Inhalte
  tatsächlich aktualisiert werden.
