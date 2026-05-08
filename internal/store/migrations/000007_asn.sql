-- 000007_asn.sql
--
-- Add per-flow ASN fields. NetFlow v9 / IPFIX carry source AS in
-- field ID 16 and destination AS in field ID 17 (RFC 3954 §8 and
-- RFC 7012 §5.1). 32-bit AS numbers are now standard (RFC 6793).
--
-- Forward-only: existing rows get 0 in both columns (ClickHouse
-- ALTER ADD COLUMN default). The /api/top/asn endpoint and ASN
-- tab on Flows treat 0 as "unknown / not exported".

ALTER TABLE flows
    ADD COLUMN IF NOT EXISTS src_as UInt32 DEFAULT 0,
    ADD COLUMN IF NOT EXISTS dst_as UInt32 DEFAULT 0;
