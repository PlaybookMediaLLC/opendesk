-- A canvas's own version (contracts/wiki.md "Canvas"): +1 on every save that changes its cards
-- or connections. A save that names the version it edited from is refused when the canvas moved
-- on since. A constant default adds the column without rewriting the table.
ALTER TABLE wiki_pages ADD COLUMN canvas_version integer NOT NULL DEFAULT 1;
