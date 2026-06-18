# Yardmaster

Open-source playbook execution and fleet orchestration platform. Alternative to AWX and Semaphore.

## Status

Pre-alpha. Private development. Public release pending.

## Concept

Yardmaster orchestrates work across a fleet the way a railroad yardmaster orchestrates engines across tracks. Scheduling, switching, classifying, dispatching.

Internal vocabulary follows the rail-yard metaphor:

| Yard term     | Yardmaster meaning            |
|---------------|-------------------------------|
| Engine        | Playbook                      |
| Track         | Pipeline                      |
| Mainline      | Default execution path        |
| Spur          | Branch                        |
| Switchyard    | Job router                    |
| Roundhouse    | Execution environment         |
| Manifest      | Inventory                     |
| Consist       | Ordered execution plan        |
| Siding        | Staging area                  |
| Interlock     | Policy gate                   |
| Brakeman      | Cancellation service          |
| Dispatcher    | Scheduler                     |
| Yardgoat      | Lightweight worker            |
| Trainmaster   | Senior operator role          |
| Stationmaster | Tenant administrator          |

## License

Apache-2.0. See `LICENSE`.
