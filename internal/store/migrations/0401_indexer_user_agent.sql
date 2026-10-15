-- 0401_indexer_user_agent: an optional User-Agent an indexer row sends on every request
-- (caps, search, NZB fetch). Some indexers (NZBgeek) refuse requests with none.
-- NULL = not set: the client's default User-Agent is sent, as before.

ALTER TABLE indexers ADD COLUMN user_agent TEXT;
