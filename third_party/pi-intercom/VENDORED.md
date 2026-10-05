# pi-intercom: eigene Kopie im PoC

Dieses Verzeichnis ist eine **angepasste Kopie** des npm-Pakets `pi-intercom`. Sie liegt im Repo und
ersetzt beim Bau des pi-Abbilds die Dateien, die `pi install npm:pi-intercom@0.15.0` anlegt (die
Abhängigkeit `tsx` kommt weiter aus npm). Anpassungen passieren nur hier.

## Herkunft

| | |
|---|---|
| Paket | `pi-intercom` auf npm (<https://www.npmjs.com/package/pi-intercom>) |
| Version | **0.15.0** |
| Integrity (`npm pack --json`) | `sha512-Dy2BZdkqbOqk10Ac4J6u0E3bYOxXOoNqE0wJl/7aIMVz+wOtyS3BbG5iDsjHOuP0wlfqDILu+03/MSbg0Ik5WQ==` |
| shasum | `7a38cabfec28ecd622dca7615ca2a4191cb9c40e` |
| Übernommen am | 30.09.2026 |
| Inhalt | der Ordner `package/` aus `npm pack pi-intercom@0.15.0` (37 Dateien) |

**Was fremd und was eigen ist, zeigt die Git-Historie:** Commit `4ce9ca6` enthält das Paket
unverändert (byteweise gleich dem Tarball), der folgende Commit nur die eigenen Änderungen:

```bash
git diff 4ce9ca6 HEAD -- poc/third_party/pi-intercom
```

## Lizenz

MIT, Copyright bei den Autoren von pi-intercom; der Lizenztext liegt unverändert in
[`LICENSE`](LICENSE). Die lokalen Änderungen stehen unter derselben Lizenz.

## Änderungen

1. **`index.ts`, `deliverIncomingBrokerMessage`: Nachrichten an beschäftigte Sitzungen ohne
   Oberfläche werden eingeschleust statt abgelehnt.** Im Original antwortet eine Sitzung ohne UI
   (`hasUI` falsch), die gerade arbeitet, dem Absender automatisch „This agent is running in
   non-interactive mode and cannot respond …“ und verwirft die Nachricht. Subagenten von
   pi-subagents sind solche Sitzungen und fast immer beschäftigt; direkte Nachrichten zwischen
   laufenden Subagenten kämen damit nie an (am 30.09.2026 am System beobachtet: `delivered: true`
   beim Absender, keine Nachricht im Verlauf des Empfängers). Jetzt gilt für sie derselbe Weg wie für
   Sitzungen mit UI: Einschleusen per `steer`, also nach den laufenden Werkzeugen und vor dem nächsten
   Modellaufruf. Mit `PI_INTERCOM_REFUSE_WHEN_BUSY=1` gilt wieder das Verhalten des Originals.
   Belegt durch `TestSlotSubagentIntercom` (`poc/internal/worker/talk_docker_test.go`).

## Hinweis zum eigenen Repo (05.10.2026)

Das Gateway liegt seit dem 05.10.2026 in einem eigenen Repo. Die oben genannten Commits stehen im
Masterarbeits-Repo (`poc/third_party/…`); hier kam die Kopie mit dem ersten Commit **bereits angepasst**
herein. Die eigenen Änderungen lassen sich trotzdem jederzeit nachprüfen: `npm pack` mit der oben
genannten Version holen, entpacken und mit `diff -r package/ <dieses Verzeichnis>` vergleichen.
