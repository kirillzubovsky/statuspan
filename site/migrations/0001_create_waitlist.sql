CREATE TABLE IF NOT EXISTS waitlist (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  created_at TEXT NOT NULL,
  name TEXT NOT NULL,
  email TEXT NOT NULL UNIQUE,
  company TEXT NOT NULL DEFAULT '',
  users TEXT NOT NULL DEFAULT '',
  comments TEXT NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS idx_waitlist_created_at ON waitlist(created_at);
