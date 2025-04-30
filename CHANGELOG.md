## 0.0.4 (2025-04-30)

### Features

- Return error when transaction set relay fails

### Fixes

- Update coreutils to v0.0.4

#### Export 'missing block at index' error

The "missing block at index" error is used in multiple places to detect that a chain migration happened so that the app knows to reset whatever state it stored previously.  This PR exports it so that looking at the error string is not necessary.