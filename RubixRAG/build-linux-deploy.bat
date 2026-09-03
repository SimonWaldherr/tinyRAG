@echo off
setlocal

rem ===========================================================================
rem R3 - Cross-Compile fuer Linux/amd64 + Deploy-Paket zusammenstellen
rem ===========================================================================
rem Baut die R3-Binary fuer den Zielserver (linux/amd64, kein CGO im Projekt,
rem daher ohne C-Toolchain moeglich) und stellt alles, was fuer ein Deployment
rem noetig ist, in einem frischen "upload"-Ordner zusammen:
rem
rem   upload\R3                  - die Linux-Binary
rem   upload\prompts\             - Systemprompt/Skills/department_rules.json
rem                                 (wird zur Laufzeit vom Dateisystem gelesen,
rem                                 ist NICHT wie web/* in die Binary eingebettet)
rem   upload\DEPLOY-HINWEISE.txt  - was danach auf dem Server noch zu tun ist
rem   upload\R3-deploy-*.zip      - dasselbe als ein Zip zum Hochladen
rem
rem Bewusst NICHT enthalten: settings.json, der Speicherordner (storage.path,
rem z.B. r3-data\), r3-originals\ - das ist echter Laufzeit-/Konfigurations-
rem zustand (eigene Dev-Settings bzw. bei einem Update der Produktivinstanz:
rem deren echte Daten), kein Build-Artefakt, und darf nicht versehentlich eine
rem laufende Instanz ueberschreiben.
rem
rem Dieses Skript geht davon aus, dass es im Projekt-Root liegt (neben main.go).
rem ===========================================================================

set "GO_SDK=C:\Users\waldherr\go-sdk\go\bin"
set "PROJECT_DIR=%~dp0"
set "OUTPUT_DIR=%PROJECT_DIR%upload"
set "BINARY_NAME=R3"

echo.
echo === R3 - Linux-Build + Deploy-Paket ===
echo Projektverzeichnis: %PROJECT_DIR%
echo Ausgabe:            %OUTPUT_DIR%
echo.

if not exist "%GO_SDK%\go.exe" (
    echo FEHLER: Go-SDK nicht gefunden unter "%GO_SDK%".
    echo Bitte GO_SDK am Anfang dieser Datei anpassen.
    goto :fail
)

set "PATH=%GO_SDK%;%PATH%"

echo Verwende Go-Toolchain:
go version
if errorlevel 1 goto :fail
echo.

rem --- Alten Upload-Ordner leeren, damit nichts Veraltetes liegen bleibt ---
if exist "%OUTPUT_DIR%" (
    echo Leere vorhandenen Upload-Ordner...
    rmdir /s /q "%OUTPUT_DIR%"
)
mkdir "%OUTPUT_DIR%"
if errorlevel 1 goto :fail

rem --- Cross-Compile: linux/amd64, kein CGO (Projekt hat keine C-Abhaengigkeiten) ---
rem -trimpath entfernt lokale Dateipfade dieser Build-Maschine aus der
rem Binary; -ldflags="-s -w" entfernt DWARF-Debug-Tabellen/Symboltabelle
rem (verifiziert: ~35 MB -> ~26 MB). Fuer ein Deployment-Artefakt ein
rem klarer Gewinn, ohne Funktionsverlust - kostet nur Datei/Zeilenangaben
rem in einem Panic-Stacktrace und delve-Unterstuetzung, beides fuer eine
rem Produktivinstanz ohnehin nicht relevant (siehe Makefiles "build" vs
rem "release" fuer denselben Trade-off beim lokalen Dev-Build).
echo.
echo Baue %BINARY_NAME% fuer linux/amd64 ...
pushd "%PROJECT_DIR%"
set "GOOS=linux"
set "GOARCH=amd64"
set "CGO_ENABLED=0"
go build -trimpath -ldflags="-s -w" -o "%OUTPUT_DIR%\%BINARY_NAME%" .
set "BUILD_RESULT=%ERRORLEVEL%"
popd

if not "%BUILD_RESULT%"=="0" (
    echo.
    echo FEHLER: Build fehlgeschlagen ^(Exit-Code %BUILD_RESULT%^). Es wurde nichts kopiert.
    goto :fail
)
echo Build erfolgreich.

rem --- prompts\ mitkopieren ---
if exist "%PROJECT_DIR%prompts" (
    echo.
    echo Kopiere prompts\ ...
    xcopy "%PROJECT_DIR%prompts" "%OUTPUT_DIR%\prompts\" /E /I /Q /Y >nul
) else (
    echo.
    echo WARNUNG: prompts\ nicht gefunden - wird nicht mit ins Paket gelegt.
)

rem --- init.d-Skript + Default-Config mitkopieren (nur beim ersten Setup
rem     auf einem neuen Server noetig, schadet bei einem reinen
rem     Binary-Update aber nicht - siehe DEPLOY-HINWEISE.txt) ---
if exist "%PROJECT_DIR%deploy\r3.init" (
    echo.
    echo Kopiere deploy\ ^(init.d-Skript + Default-Config^) ...
    xcopy "%PROJECT_DIR%deploy" "%OUTPUT_DIR%\deploy\" /E /I /Q /Y >nul
)

rem --- Kurze Deploy-Hinweise mit ins Paket legen ---
(
    echo R3 - Deploy-Paket
    echo Erstellt: %DATE% %TIME%
    echo.
    echo Enthalten:
    echo   - %BINARY_NAME%   ^(Linux/amd64, statisch gelinkt, kein CGO^)
    echo   - prompts\        ^(Systemprompt + Skills + ggf. department_rules.json^)
    echo   - deploy\r3.init     ^(init.d-Skript, siehe Punkt 6^)
    echo   - deploy\r3.default  ^(zugehoerige Config-Vorlage, siehe Punkt 6^)
    echo.
    echo Vor dem ersten Start auf dem Zielserver:
    echo   1. Alles hochladen ^(z. B. FileZilla/SFTP^) - z. B. nach /mnt/application/R3/
    echo   2. chmod +x %BINARY_NAME%   ^(Ausfuehrungsbit wird beim Upload NICHT automatisch gesetzt^)
    echo   3. Bei Azure OpenAI: AZURE_OPENAI_API_KEY als Umgebungsvariable setzen,
    echo      NICHT in eine settings.json eintragen.
    echo   4. Falls dort schon eine Instanz laeuft: NICHT ueberschreiben -
    echo      settings.json, den Speicherordner ^(storage.path, z. B. r3-data\^)
    echo      und r3-originals\ - das ist echter Laufzeit-Zustand, kein Build-Artefakt.
    echo   5. Manueller Start zum Test: ./%BINARY_NAME% -addr :8090 -storage-path r3-data ...
    echo      ^(alle Flags: README.md "Running" / ANLEITUNG.md Abschnitt 3^)
    echo   6. Als Dienst einrichten ^(einmalig, danach reicht "service r3 restart"^):
    echo        cp deploy/r3.init /etc/init.d/r3 ^&^& chmod +x /etc/init.d/r3
    echo        cp deploy/r3.default /etc/default/r3   ^(Pfade/Flags dort anpassen^)
    echo        chown root:root /etc/default/r3 ^&^& chmod 600 /etc/default/r3
    echo        update-rc.d r3 defaults
    echo        service r3 start
    echo      Verbose-Logging ^(genauer Request-/Modell-/Import-Log^) an- und
    echo      ausschalten: R3_VERBOSE=1 bzw. 0 in /etc/default/r3, dann
    echo      "service r3 restart". Log-Ausgabe landet in R3_LOGFILE
    echo      ^(Default: /mnt/application/R3/r3.log^) - "tail -f" darauf.
) > "%OUTPUT_DIR%\DEPLOY-HINWEISE.txt"

rem --- Optional: alles zusaetzlich als ein Zip fuer den Upload (best effort) ---
echo.
echo Erstelle Zip-Archiv...
set "ZIP_NAME=R3-deploy-%DATE:~-4,4%%DATE:~-7,2%%DATE:~-10,2%.zip"
set "ZIP_NAME=%ZIP_NAME: =%"
set "ZIP_SOURCES=%OUTPUT_DIR%\%BINARY_NAME%','%OUTPUT_DIR%\prompts','%OUTPUT_DIR%\DEPLOY-HINWEISE.txt"
if exist "%OUTPUT_DIR%\deploy" set "ZIP_SOURCES=%ZIP_SOURCES%','%OUTPUT_DIR%\deploy"
powershell -NoProfile -Command "Compress-Archive -Path '%ZIP_SOURCES%' -DestinationPath '%OUTPUT_DIR%\%ZIP_NAME%' -Force" 2>nul
if exist "%OUTPUT_DIR%\%ZIP_NAME%" (
    echo Zip erstellt: %OUTPUT_DIR%\%ZIP_NAME%
) else (
    echo Zip konnte nicht erstellt werden ^(nicht kritisch, Ordner ist trotzdem komplett^).
)

echo.
echo === Fertig ===
echo Deploy-Paket liegt in: %OUTPUT_DIR%
echo.
dir "%OUTPUT_DIR%"
echo.
pause
exit /b 0

:fail
echo.
echo Abgebrochen.
pause
exit /b 1
