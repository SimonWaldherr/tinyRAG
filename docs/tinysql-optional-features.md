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
- **Technische Kennungen:** Bei Produkt-, Fehler- oder Versionskennungen nutzt
  tinyRAG zusätzlich `FTS_SEARCH` als begrenzte Kandidatenquelle. Die Eingabe
  wird dabei in sichere Literale zerlegt, Rollen- und ACL-Filter gelten auch
  dort, und bei fehlender Unterstützung bleibt die Vektorsuche unverändert.
- **Geodaten-Import:** Erlaubt Uploads von GeoJSON, KML und OpenStreetMap XML
  über den Open-Data-Tab. Jeder Import erhält R3-Quellmetadaten und wird
  idempotent über den Dateinamen aktualisiert.

Audit und Verschlüsselung werden beim Start eingerichtet. Änderungen an diesen
beiden Optionen benötigen daher einen Neustart.

## Administration und Integrität

- **Portabler Snapshot:** `GET /api/debug/database-snapshot` liefert für
  Administratoren einen konsistenten GOB-Snapshot der Datenbank. Er funktioniert
  unabhängig vom gewählten Storage-Modus und ist für externe Backups gedacht.
- **Audit-Statementtexte:** Die dynamische, schreibgeschützte SQL-Abfrage des
  Agenten nutzt `ExecSQL` mit Audit-Kontext. Bei aktivem Audit steht daher das
  originale SQL-Statement statt nur eines Statement-Typs im Hash-Protokoll.
