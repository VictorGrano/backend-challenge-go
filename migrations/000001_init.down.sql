DROP TABLE IF EXISTS outbox_events;
DROP FUNCTION IF EXISTS outbox_events_protect();

DROP TABLE IF EXISTS inbox_messages;

DROP TABLE IF EXISTS wallet_ledger_entries;
DROP FUNCTION IF EXISTS wallet_ledger_entries_immutable();

DROP TABLE IF EXISTS wager_transactions;
DROP FUNCTION IF EXISTS wager_transactions_protect();

DROP TABLE IF EXISTS wallets;
