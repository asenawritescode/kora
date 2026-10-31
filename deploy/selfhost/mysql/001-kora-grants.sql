-- This single-database-server example is dedicated to Kora. The Engine
-- provisions separate tenant databases on this server, so its service account
-- needs database-level DDL/DML rights across schemas. Do not reuse this grant
-- on a shared server containing unrelated applications.
GRANT ALL PRIVILEGES ON *.* TO 'kora'@'%';
