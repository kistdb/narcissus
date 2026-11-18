CREATE INDEX orders_status ON orders (status) WHERE deleted_at IS NULL;
CREATE INDEX orders_total ON orders (total);
