# R3 — LIESMICH

*Für alle, die wissen wollen, was R3 ist und wo das Projekt steht — ohne
Technik-Vorwissen. Für Betrieb/Konfiguration siehe [`ANLEITUNG.md`](ANLEITUNG.md),
für technische Details [`README.md`](README.md), für den aktuellen
Projektstatus [`docs/PROJEKTPLAN.md`](docs/PROJEKTPLAN.md).*

## Was ist R3 in einem Satz?

R3 ("Rubix Ranked RAG") ist ein internes Werkzeug, das Fragen zu unseren
eigenen Unterlagen — E-Mails, Dateien, SharePoint, Teams, Tickets — mit
einer konkreten Quellenangabe beantwortet, statt frei zu raten.

## Warum gibt es das?

Ein normales KI-Sprachmodell "erfindet" Antworten aus dem, was es beim
Training gesehen hat — es kennt unsere Postfächer, Ablagen und internen
Absprachen nicht und kann sich Dinge ausdenken, die plausibel klingen,
aber falsch sind. R3 macht daraus etwas Verlässlicheres: Bevor R3 eine
Frage beantwortet, sucht es zuerst in den tatsächlich importierten
Unterlagen nach den passendsten Stellen und zeigt in der Antwort, welche
konkrete E-Mail, welches Dokument oder welches Ticket sie belegt. Das
spart Suchzeit und macht die Antwort nachprüfbar, statt sie einfach zu
glauben.

## Zwei Ziele, zwei Termine

Aktuell laufen zwei Dinge parallel, mit unterschiedlichem Zeithorizont:

- **Mail-Assistent, bis KW 30** *(kurzfristig neu priorisiert)*: liest
  E-Mails, speichert sie und schlägt auf Basis alter E-Mails
  Antwortentwürfe vor, die am Server abgelegt werden. Läuft direkt am
  Postfach, ohne eigene Bedienoberfläche für diese Kernfunktion — das
  bestehende Web-Interface bleibt aber wichtig zum Testen und zum
  Verwalten dessen, was über das Postfach läuft. Auch hier gilt: **nichts
  wird automatisch verschickt**, ein Mensch prüft jeden Entwurf.
- **Wissensplattform-MVP, bis KW 37** *(ursprünglicher Fokus)*: die
  zentrale Such-/Chat-Oberfläche über SharePoint, ein priorisiertes
  Ticket-System, ein Wiki und eine zentrale Dateiablage, mit einem Pilot
  bei ausgewählten Kolleg:innen aus Vertrieb, Finance, IT und Logistik
  vor dem eigentlichen Start.

Details, Zeitplan-Einschätzung und was dafür jeweils noch fehlt: siehe
[Projektplan](docs/PROJEKTPLAN.md).

## Wo steht das Projekt heute?

**R3 läuft bereits produktiv** als erste funktionsfähige Version (MVP) auf
einem internen Server. Es ist kein reiner Prototyp mehr, sondern
tatsächlich im Einsatz — allerdings noch mit ein paar offenen Punkten, die
vor einem größeren Rollout geklärt werden sollten (siehe unten und den
[Projektplan](docs/PROJEKTPLAN.md)).

Bereits angebunden bzw. nutzbar:

- Postfach-Exporte (PST) und einzelne Dateien (Word, Excel, PDF, …)
- SharePoint, Outlook/Exchange, IMAP-Postfächer, Microsoft Teams
- Confluence, Jira, Freshservice-Tickets
- Beliebige Webseiten (per Link)
- Eine Live-Abfrage-Funktion für ausgewählte Datenbank-Tabellen sowie für
  beliebige, vom Admin freigegebene REST-Schnittstellen (z. B. interne
  Systeme, auch solche mit einem intern ausgestellten statt einem
  öffentlichen Zertifikat)
- Für SharePoint zusätzlich eine **Live-Suche**: R3 kann bei Bedarf direkt
  in einer freigegebenen SharePoint-Site nachschauen, statt nur in bereits
  importierten Dateien — nützlich, wenn sich eine Datei kürzlich geändert
  hat oder noch gar nicht importiert wurde. Ebenfalls neu: einzelne bereits
  importierte SharePoint-Quellen lassen sich gezielt "neu laden", ohne den
  gesamten Import erneut anzustoßen.
- Ein Anmeldeverfahren über unsere bestehenden Windows-Zugangsdaten (AD),
  mit dem sich steuern lässt, welche Abteilung welche Inhalte sehen darf
  (z. B. ein Postfach nur für Vertrieb sichtbar machen)
- Ein Vorschlag für Antwortentwürfe zu eingehenden E-Mails — R3 schlägt
  einen Text vor, ein Mensch prüft und versendet ihn. **R3 versendet
  grundsätzlich nie selbst etwas.** Für Outlook/Exchange-Postfächer inzwischen
  auch direkt im eigenen Postfach nutzbar, ohne Text hin- und herzukopieren.
- Fragen können auch mit einem angehängten Bild oder Dokument gestellt
  werden (z. B. ein Foto eines Formulars); die Weboberfläche ist zusätzlich
  zu Deutsch auch auf Englisch, Französisch und Italienisch nutzbar.
- Die Wahl des zugrunde liegenden KI-Sprachmodells ist nicht auf einen
  einzigen Anbieter festgelegt — je nach Bedarf lassen sich verschiedene
  Anbieter anbinden, ohne an einen einzelnen gebunden zu sein.

## Worauf sollte Management aktuell achten?

Ehrlich und kurz zusammengefasst, ohne Technik-Details (die stehen im
[Projektplan](docs/PROJEKTPLAN.md)):

1. **Der Server, auf dem R3 läuft, ist aktuell nicht zusätzlich
   abgeschottet** — es fehlt eine Firewall/ein vorgeschalteter
   Schutzmechanismus. Solange R3 nur intern erreichbar ist, ist das
   Risiko begrenzt, sollte aber vor einer breiteren Nutzung behoben
   werden.
2. **R3 läuft aktuell mit zu weitreichenden Systemrechten** (wie ein
   Administrator-Konto), statt mit einem eigenen, eingeschränkten
   Dienstkonto. Ebenfalls ein offener Punkt vor breiterem Einsatz.
3. **Einige Anbindungen (SharePoint, Exchange, Confluence, Jira, die
   Datenbank-Abfrage) wurden bisher nur in einer Testumgebung geprüft**,
   noch nicht gegen unsere echten Systeme. Das heißt nicht, dass sie
   nicht funktionieren — nur, dass ein erster echter Praxistest noch
   aussteht, bevor man sich voll darauf verlässt.
4. **Es gibt noch keinen automatisierten Update-Prozess** — neue Versionen
   werden aktuell händisch auf den Server kopiert. Für den aktuellen
   Projektumfang ist das vertretbar, sollte mit wachsender Nutzung aber
   überdacht werden.

Keiner dieser Punkte ist ein Grund, das Projekt zu stoppen — sie sind der
normale Weg von "funktioniert" zu "produktionsreif" und sind im
[Projektplan](docs/PROJEKTPLAN.md) als nächste Schritte eingeplant.

## Warum wir langfristig auf offene Schnittstellen (MCP) setzen sollten

R3 kann heute Fragen zu Unterlagen beantworten, die vorher eingelesen
wurden — also zu einem Schnappschuss vom letzten Import, nicht zum
aktuellen Stand. Für die meisten Unterlagen reicht das (ein Dokument von
letzter Woche ändert sich selten), aber nicht für alles: "Ist Ticket
#4711 noch offen?" oder "Was zeigt die Datenbank gerade an?" braucht eine
Antwort in Echtzeit, nicht den Stand vom letzten Import.

Der erste Schritt dahin sind fest hinterlegte, von einem Menschen
geprüfte Live-Abfragen (wie die gerade eingebauten SQL-Abfrage-Vorlagen)
— R3 darf dann gezielt aktuelle Daten abrufen, aber nur genau das, was
vorher freigegeben wurde, nicht irgendetwas Beliebiges.

Der größere, langfristige Schritt ist, dafür einen offenen,
branchenweiten Standard zu nutzen (das "Model Context Protocol", kurz
MCP) statt für jede neue Datenquelle wieder eigene Entwicklungsarbeit zu
brauchen. Das lohnt sich aus zwei Gründen:

1. **Neue Datenquellen ließen sich größtenteils durch Konfiguration statt
   durch Programmierung anbinden** — passt genau zum größeren Ziel,
   Wissensinseln abzubauen (siehe [Projektplan](docs/PROJEKTPLAN.md)),
   nur eben nicht nur einmalig für den MVP, sondern dauerhaft, für jede
   künftige Quelle.
2. **R3 würde von einer Insel-Lösung zu einem wiederverwendbaren
   Baustein** — andere KI-Initiativen in der Unternehmensgruppe könnten
   R3s Wissenssuche mitnutzen, und R3 könnte umgekehrt auf zentral
   bereitgestellte Werkzeuge zugreifen, die IT oder andere Teams
   unabhängig von R3 pflegen, statt dass jedes Team seine eigene,
   isolierte Anbindung baut.

Den technischen Plan dazu (was genau, warum, in welcher Reihenfolge)
hält [`docs/MCP_CONNECTORS_PLAN.md`](docs/MCP_CONNECTORS_PLAN.md) fest —
bewusst nur als Planungsgrundlage, noch nicht umgesetzt.

## Wer kann was dazu sagen?

- **Patrick Otterski** (Head of AI & Process Excellence) — Projekt-Sponsor,
  entscheidet über Umfang, Priorität, Termine und den Go-Live; erste
  Adresse für Management-Fragen zum Projekt.
- **Simon Waldherr** — verantwortet die operative Umsetzung: Technik,
  Architektur, alle Anbindungen.
- **Lena Heitzer** — Ansprechpartnerin für Fachbereiche, Kommunikation
  und die Koordination mit den Key Usern im Pilot.

Die vollständige Rollenverteilung (inkl. Vertrieb, Finance, IT,
Logistik, Datenarchitektur) steht im [Projektplan](docs/PROJEKTPLAN.md).

## Mehr lesen

- [`docs/PROJEKTPLAN.md`](docs/PROJEKTPLAN.md) — Phasenstatus, Risiken,
  nächste Schritte
- [`ANLEITUNG.md`](ANLEITUNG.md) — technische Anleitung auf Deutsch
  (Betrieb, Konfiguration, Erweiterung)
- [`README.md`](README.md) — vollständige technische Referenz (Englisch)
