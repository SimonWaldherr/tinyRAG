# Lokale Spracheingabe (Whisper)

**Verbindungsrolle:** Server (HTTP) und lokaler Prozess (Whisper/FFmpeg)  
**Datenfluss:** Push (Audio) → Response (Transkript)  
**Implementierung:** [handlers_voice.go](../handlers_voice.go), [whisper.go](../whisper.go)

## Endpunkt

`POST /api/voice/transcribe` mit `multipart/form-data` und dem Feld `audio`.
Der Endpunkt verwendet dieselbe optionale API-Key-Regel wie `/api/ask`.

Antwort:

```json
{"text":"Transkribierte Frage","language":"de"}
```

## Verarbeitung

Audio wird nur in einer temporären Datei verarbeitet und danach gelöscht. Nicht-
WAV-Aufnahmen (z. B. Browser-WebM/Opus) werden mit dem konfigurierten FFmpeg
in mono 16-kHz-WAV konvertiert. Danach wird das konfigurierte Whisper-
kompatible CLI im whisper.cpp-Stil aufgerufen:

`-m <model> -f <wav> -l <language> -otxt -of <output>`

R3 lädt weder Binary noch Modell herunter und speichert Audio/Transkript nicht
als Quelle oder Chat-Historie. `allow_shell_exec` muss aktiviert sein.

## Einstellungen

- `import.whisper_bin` – Binary oder Pfad, Standard `whisper-cli`
- `import.whisper_model` – lokale Modell-Datei, z. B. `ggml-small.bin`
- `import.whisper_language` – optional, z. B. `de`
- `import.whisper_timeout_seconds` – 0 = 120 Sekunden, maximal 600
- `import.ffmpeg_bin` – wird für Browser-Audioformate verwendet

Das Audio-Upload-Limit ist 32 MiB pro Request; die effektive Audiodatei wird
zusätzlich durch `import.max_file_mb` begrenzt.
