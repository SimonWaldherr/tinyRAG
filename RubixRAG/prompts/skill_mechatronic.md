---
name: Mechatronik
description: Fachwissen zu mechatronischen Komponenten, Antriebstechnik, Steuerungssystemen und Wartungsverfahren
enabled: true
tags: [mechatronik, antrieb, motor, getriebe, steuerung, sps, sensor, wartung, ersatzteil, instandhaltung, störung, reparatur]
---

## Antriebstechnik

Bei Fragen zu Antriebskomponenten (Motoren, Getriebe, Kupplungen):
- Nennleistung, Drehzahl und Drehmoment immer vom Typenschild ablesen
- Betriebsstunden und nächste Wartung aus dem Maschinenbuch oder ERP prüfen
- Vor jeder Demontage: Sicherheitsabschaltung nach LOTO-Verriegelungsplan einleiten (Energie freisetzen und sichern)

### Typische Störungsbilder Elektromotor

| Symptom | Mögliche Ursache | Erste Maßnahme |
|---|---|---|
| Überhitzung | Überlast, Kühlung verstopft | Auslastung prüfen, Lüfter reinigen |
| Lagergeräusch | Lagerverschleiß, Schmierstoffmangel | Nachschmieren oder Austausch planen |
| Anlaufproblem | Kondensator defekt (1-phasig), Wicklungsschluss | Kondensator messen, Wicklungswiderstand prüfen |
| Vibration | Unwucht, Fluchtungsfehler | Kupplung und Wellenausrichtung kontrollieren |

## Steuerungstechnik (SPS)

Standardvorgehen bei SPS-Störmeldungen:
1. Fehlercode und Zeitstempel im HMI dokumentieren (Screenshot oder Foto anfertigen)
2. Diagnosepuffer in der SPS-Software auslesen (TIA Portal: Online → Diagnosepuffer; Step 7: CPU-Diagnosepuffer)
3. Betriebsmeldungen der letzten 24 Stunden im HMI/Logbuch prüfen
4. Erst nach Rücksprache mit dem zuständigen Elektriker in SPS-Konfiguration oder Parametrierung eingreifen

Wichtig: Veränderungen an SPS-Programmen immer als neue Version sichern (Versionsnummer, Datum, Kürzel im Kommentarblock).

## Wartungsintervalle (Richtwerte)

| Komponente | Intervall (Betriebsstunden) | Wartungsmaßnahme |
|---|---|---|
| Elektromotor (geschlossen) | 4.000 Bh | Lager kontrollieren, Oberfläche auf Verschmutzung prüfen |
| Elektromotor (IP23) | 2.000 Bh | Lager fetten oder tauschen, Wicklung reinigen |
| Stirnradgetriebe | 5.000 Bh | Ölstand und Ölprobe, Dichtungen auf Undichtigkeit prüfen |
| Pneumatikzylinder | 1.000 Bh | Dichtungen und Führungsbuchsen kontrollieren |
| Linearführungen (Kugelschienen) | 250 Bh | Schmierung nach Schmierplan |
| Frequenzumrichter | 2 Jahre | Lüfter, Kondensatoren und Klemmen prüfen |

## Ersatzteilmanagement

- Kritische Ersatzteile werden im ERP unter Materialgruppe **INSTA** (Instandhaltung) geführt
- Mindestbestand für A-Teile (betriebskritische Komponenten) im ERP hinterlegen
- Bestellanforderung (BANF) wird bei Unterschreitung des Meldebestands automatisch erzeugt
- Herstellerersatzteile vs. Alternativlieferant: Freigabe durch Instandhaltungsleiter erforderlich

## Eskalationsweg Maschinenausfall

1. Sofortmeldung an Schichtführer und Instandhaltung
2. Instandhaltung bewertet Ausfallzeit und Ersatzteilbedarf
3. Bei mehr als 4 Stunden geplantem Ausfall: Produktionsleiter informieren, Ersatzkapazität prüfen
4. Externe Servicetechniker (Hersteller) nur über Instandhaltungsleiter beauftragen
