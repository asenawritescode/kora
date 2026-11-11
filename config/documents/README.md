# documents

A file register with expiry dates and a sign-off status: Document Category, Document and
Document Version. Roles: **Documents Manager** (everything, decides sign-off) and
**Documents User** (reads, creates and edits the documents they created; cannot delete).
One view, **Documents Library** at `/documents` (documents with their expiry date and status,
and a second table of documents waiting for sign-off), and a documents overview report
(documents by status and by category).

- Category names are unique. A document carries its status (Draft, Active, Archived), an
  expiry date, one attached file, who keeps it and who should approve it.
- Once a document's sign-off is Approved or Rejected, its title, category and file are frozen.
- A document version records a version number, a file, what changed and who uploaded it.

Limits: file register with expiry dates and a sign-off status. No e-signature, no full-text
search, no automatic expiry reminders. Sign-off is a status a person sets; nothing routes the
document to the approver, and the Engine does not stop a Documents User setting it. Version
numbers are typed by hand and not checked for gaps or repeats. Documents Users see only the
documents they created (row scope applies to read and write together).

Checks: `go test ./internal/site -run TestDocuments`, `go test ./internal/analytics -run TestDocuments`
and `go test -tags integration ./internal/api -run TestDocuments` (needs MySQL, set `KORA_TEST_DSN`).
