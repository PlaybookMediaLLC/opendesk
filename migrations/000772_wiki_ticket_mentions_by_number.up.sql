-- A ticket mention in the wiki is stored as its number alone (contracts/wiki.md "Mentions"):
-- the app draws the subject live for a reader who may see the ticket. Stored bodies, their
-- revisions and canvas cards are brought to that form; hd.SanitizeWiki keeps it from now on.
-- The mention's start tag is the canonical form hd.SanitizeWiki writes, and its text holds
-- no tags (the editor writes a mention as an atom with plain text).
UPDATE wiki_pages SET body_html = regexp_replace(body_html,
    '(<a data-type="ticket" data-id="[0-9a-fA-F-]{36}" href="/tickets/([0-9]{1,18})"[^>]*>)[^<]*</a>', '\1#\2</a>', 'g')
    WHERE body_html LIKE '%data-type="ticket"%';
UPDATE wiki_page_revisions SET body_html = regexp_replace(body_html,
    '(<a data-type="ticket" data-id="[0-9a-fA-F-]{36}" href="/tickets/([0-9]{1,18})"[^>]*>)[^<]*</a>', '\1#\2</a>', 'g')
    WHERE body_html LIKE '%data-type="ticket"%';
-- A canvas's text cards hold the same HTML inside JSON strings (quotes escaped).
UPDATE wiki_pages SET canvas = regexp_replace(canvas::text,
    '(<a data-type=\\"ticket\\" data-id=\\"[0-9a-fA-F-]{36}\\" href=\\"/tickets/([0-9]{1,18})\\"[^>]*>)[^<]*</a>', '\1#\2</a>', 'g')::jsonb
    WHERE kind = 'canvas' AND canvas::text LIKE '%data-type=\\"ticket\\"%';
-- Their search words are read again from the cards (the API keeps them as it saves a canvas).
UPDATE wiki_pages p SET canvas_text = left(COALESCE((
    SELECT string_agg(s, E'\n') FROM (
        SELECT btrim(regexp_replace(regexp_replace(COALESCE(n->>'html', ''), '<[^>]+>', ' ', 'g'), '\s+', ' ', 'g')) AS s
            FROM jsonb_array_elements(COALESCE(p.canvas->'nodes', '[]'::jsonb)) n
        UNION ALL SELECT btrim(COALESCE(n->>'label', '')) FROM jsonb_array_elements(COALESCE(p.canvas->'nodes', '[]'::jsonb)) n
        UNION ALL SELECT btrim(COALESCE(n->>'title', '')) FROM jsonb_array_elements(COALESCE(p.canvas->'nodes', '[]'::jsonb)) n
        UNION ALL SELECT btrim(COALESCE(n->>'description', '')) FROM jsonb_array_elements(COALESCE(p.canvas->'nodes', '[]'::jsonb)) n
        UNION ALL SELECT btrim(COALESCE(n->>'name', '')) FROM jsonb_array_elements(COALESCE(p.canvas->'nodes', '[]'::jsonb)) n
        UNION ALL SELECT btrim(COALESCE(e->>'label', '')) FROM jsonb_array_elements(COALESCE(p.canvas->'edges', '[]'::jsonb)) e
    ) t WHERE s <> ''), ''), 262144)
    WHERE kind = 'canvas' AND canvas::text LIKE '%data-type=\\"ticket\\"%';
