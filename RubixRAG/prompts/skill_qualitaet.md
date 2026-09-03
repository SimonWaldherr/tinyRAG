---
name: Qualitätsmanagement
description: QM-Prozesse, Normen (ISO 9001, IATF 16949) und Fehlerbehandlung gemäß Rubix-QM-Handbuch
enabled: true
tags: [qualitaet, qm, iso, norm, reklamation, fehler, pruefung, freigabe, audit, korrektiv, 8d, spc, aql, wareneingangskontrolle, erstmuster]
---

## Qualitätsprüfung

### Wareneingangskontrolle (WEK)

- Menge und Lieferschein gegen Bestellung prüfen (Belegkontrolle)
- Stichprobenprüfung nach **AQL-Tabelle Level II, normal** (DIN ISO 2859-1)
- Messmittel: kalibriert und Kalibrierzertifikat gültig? → Ablaufdatum im ERP prüfen
- Befund **i.O.** → Wareneingang im ERP buchen
- Befund **n.i.O.** → Sperrbestand, QM-Abteilung informieren, Lieferant benachrichtigen

### Fertigungsprüfung

- **Erstmusterprüfbericht (EMB)** bei neuen Teilen zwingend vor Serienfreigabe
- Prüflos-Dokumentation im ERP (Prüfauftrag) mit Prüfer-ID und Messwerten
- **SPC-Freigabekriterien**: Cp / Cpk ≥ 1,33 (kritische Merkmale ≥ 1,67)

## Fehlerbehandlung (8D-Methode)

| D | Schritt | Zeitrahmen |
|---|---|---|
| D1 | Teambildung (Fachexperten, QM, Produktion) | < 24 h nach Reklamation |
| D2 | Problembeschreibung (5W: Was, Wo, Wann, Wie oft, Wie viel) | < 48 h |
| D3 | Sofortmaßnahmen (Eingrenzung, Nachsortierung, Kundenschutz) | < 72 h |
| D4 | Grundursache ermitteln (Ishikawa-Diagramm, 5-Why) | < 1 Woche |
| D5 | Abstellmaßnahmen definieren | < 2 Wochen |
| D6 | Abstellmaßnahmen einführen und validieren | vereinbarter Termin |
| D7 | Wirksamkeit prüfen (Wiederholmessung, SPC) | nach Maßnahmen-Einführung |
| D8 | Abschlussbericht, Lessons Learned, Präventivmaßnahmen | < 4 Wochen |

## Reklamationsmanagement

- **Externe Reklamation (Kunde)**: Eingang im CRM erfassen, Priorität „hoch", Vertrieb sofort informieren
- **Lieferanten-Reklamation**: 8D-Report anfordern (Frist: 10 Werktage), Gegenforderung prüfen
- **Reklamationsquote** Zielwert: < 0,5 % des Umsatzes — monatlicher Review im QM-Meeting

## Audits

| Auditart | Häufigkeit | Verantwortung | Vorbereitung |
|---|---|---|---|
| Interne Audits | jährlich nach Auditprogramm | QM-Leiter | Checkliste, Auditplan |
| Lieferantenaudit | risikoorientiert (< 70 Pkt.) | Einkauf + QM | mind. 2 Wochen vorab ankündigen |
| Kundenaudit / Zertifizierungsaudit | nach Vereinbarung | QM-Leiter | mind. 4 Wochen Vorlaufzeit, Koordination QM |

## Normenbezug

- **ISO 9001:2015** — Qualitätsmanagementsystem (allgemein)
- **IATF 16949:2016** — Automotive-Ergänzung zu ISO 9001
- **DIN ISO 2859-1** — Annahmestichprobenprüfung (AQL)
- **MSA (Measurement System Analysis)** — Messsystemfähigkeit vor Serienfreigabe nachweisen
