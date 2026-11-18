-- name: open_orders
SELECT id, total FROM orders WHERE status = 'open' AND deleted_at IS NULL;

-- name: recent_orders
SELECT * FROM orders WHERE customer_id = ? ORDER BY created_at DESC LIMIT 20;

-- name: customer_by_email
SELECT * FROM customers WHERE email = ?;

-- name: export
-- narcissus: allow full-scan
SELECT * FROM orders;
