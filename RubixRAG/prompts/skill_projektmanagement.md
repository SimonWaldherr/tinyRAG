---
name: Projektmanagement
description: Prozesse, Vorlagen und Eskalationswege für das interne Projektmanagement nach Rubix-Projektleitfaden
enabled: true
tags: [projekt, projektmanagement, meilenstein, termin, statusbericht, risiko, besprechung, protokoll, budget, ressource, psp, lenkungskreis]
---

## Projektphasen (Rubix-Projektleitfaden)

1. **Initiierung** — Projektauftrag und Zieldefinition (SMART-Kriterien), Stakeholder-Analyse, Kickoff-Meeting
2. **Planung** — Projektstrukturplan (PSP), Terminplan, Ressourcen- und Budgetplanung, Risikoregister
3. **Durchführung** — wöchentliche Statusmeetings, Fortschrittsberichte, Änderungsmanagement (Change Requests)
4. **Monitoring** — Ampelstatus (RAG: Rot/Gelb/Grün), Earned Value Analysis bei budgetkritischen Projekten
5. **Abschluss** — Abnahmedokument, Lessons Learned, Projektakte schließen, Ressourcen freigeben

## Statusberichte

- Format: **Ampelbericht** (Grün / Gelb / Rot) mit Freitext zu Abweichungen und Maßnahmen
- Verteilung: jeden **Freitag bis 14:00 Uhr** per E-Mail an Projektleiter und Lenkungskreis
- Abweichungen > 10 % von geplanter Zeit oder Budget → sofortige Eskalation an Projektleiter, nicht erst im nächsten Bericht

### Ampelkriterien

| Status | Zeit | Budget | Qualität / Scope |
|---|---|---|---|
| Grün | Abweichung < 5 % | Abweichung < 5 % | Keine offenen Blocker |
| Gelb | 5–15 % | 5–15 % | Risiken identifiziert, Maßnahmen laufen |
| Rot | > 15 % oder Meilenstein gefährdet | > 15 % | Kritischer Blocker ohne Lösung |

## Risikoregister (Risikoklassen)

| Klasse | Eintrittswahrscheinlichkeit | Schadenspotenzial | Maßnahme |
|---|---|---|---|
| A — kritisch | > 50 % | hoch | Maßnahmenplan, wöchentliche Überwachung, Eskalation |
| B — bedeutend | mittel | hoch oder Eintr. > 50 % | Maßnahmenplan, monatliches Review |
| C — gering | niedrig | niedrig | Beobachten, Puffer einplanen |

## Vorlagen und Ablagestruktur

- **Projektauftrag**: SharePoint → Vorlagen → PM → `Projektauftrag_v3.dotx`
- **Risikoregister**: SharePoint → Vorlagen → PM → `Risikoregister.xlsx`
- **Meetingprotokoll**: Pflichtfelder: Datum, Teilnehmer, TOP-Liste, Beschlüsse, offene Punkte (Owner + Fälligkeitsdatum)
- **Projektakte** ablegen unter: `\\server\Projekte\<Jahr>\<ProjektNr>_<Kurzname>\`
- **Projektantrag** (ab 50.000 €): Genehmigung durch Lenkungskreis erforderlich

## Eskalationsweg

Projektmitglied → **Projektleiter** → **Bereichsleiter** → **Lenkungskreis** → Geschäftsführung

Bei Budgetüberschreitung > 20 % oder Projektstopp: sofortige Eskalation an Lenkungskreis (nicht Wochenbericht abwarten).
