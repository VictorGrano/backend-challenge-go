#!/usr/bin/env bash
API=${API:-http://localhost:8090}
KC=http://localhost:8081/realms/wagering/protocol/openid-connect/token

token() {
  curl -s -d grant_type=client_credentials -d client_id="$1" -d client_secret="$1-secret" "$KC" |
    sed -E 's/.*"access_token":"([^"]+)".*/\1/'
}

call() {
  local name=$1 want=$2
  shift 2
  local out
  out=$(curl -s -w '\n%{http_code}' "$@")
  local code=${out##*$'\n'} body=${out%$'\n'*}
  if [ "$code" = "$want" ]; then echo "ok   $code $name"; else echo "FAIL $code (want $want) $name"; fi
  echo "     $body"
  LAST=$body
}

field() { echo "$LAST" | sed -E "s/.*\"$1\":\"([^\"]+)\".*/\1/"; }

INTERNAL=$(token wallet-service)
PA=$(token provider-a)
PB=$(token provider-b)
OTHER=$(token other-audience)
PLAYER=$(cat /proc/sys/kernel/random/uuid 2>/dev/null || powershell -c "[guid]::NewGuid().ToString()")
RUN=$RANDOM$RANDOM

call "live"  200 $API/health/live
call "ready" 200 $API/health/ready

call "open wallet without token" 401 -X POST $API/wallets -d '{}'
call "open wallet with garbage token" 401 -X POST $API/wallets -H "Authorization: Bearer abc.def.ghi" -d '{}'
call "open wallet with wrong audience" 401 -X POST $API/wallets -H "Authorization: Bearer $OTHER" -d '{}'
call "open wallet as provider" 403 -X POST $API/wallets -H "Authorization: Bearer $PA" \
  -d "{\"playerId\":\"$PLAYER\",\"initialBalance\":{\"amount\":\"1000.00\",\"currency\":\"BRL\"}}"
call "open wallet with float amount" 400 -X POST $API/wallets -H "Authorization: Bearer $INTERNAL" \
  -d "{\"playerId\":\"$PLAYER\",\"initialBalance\":{\"amount\":1000.00,\"currency\":\"BRL\"}}"
call "open wallet" 201 -X POST $API/wallets -H "Authorization: Bearer $INTERNAL" \
  -d "{\"playerId\":\"$PLAYER\",\"initialBalance\":{\"amount\":\"1000.00\",\"currency\":\"BRL\"}}"
WALLET=$(field id)
call "open wallet again" 409 -X POST $API/wallets -H "Authorization: Bearer $INTERNAL" \
  -d "{\"playerId\":\"$PLAYER\",\"initialBalance\":{\"amount\":\"1.00\",\"currency\":\"BRL\"}}"

bet() {
  echo "{\"providerId\":\"$1\",\"externalTransactionId\":\"$2\",\"playerId\":\"$PLAYER\",\"walletId\":\"$WALLET\",\"roundId\":\"round-987\",\"gameId\":\"fortune-chimp\",\"kind\":\"${4:-BET}\",\"money\":{\"amount\":\"$3\",\"currency\":\"BRL\"}${5:+,\"referenceExternalTransactionId\":\"$5\"}}"
}

call "bet without idempotency key" 400 -X POST $API/wagering/transactions -H "Authorization: Bearer $PA" -d "$(bet provider-a tx-$RUN 25.00)"
call "bet as internal" 403 -X POST $API/wagering/transactions -H "Authorization: Bearer $INTERNAL" -H "Idempotency-Key: k-$RUN" -d "$(bet provider-a tx-$RUN 25.00)"
call "provider-b acting as provider-a" 403 -X POST $API/wagering/transactions -H "Authorization: Bearer $PB" -H "Idempotency-Key: provider-a:tx-$RUN" -d "$(bet provider-a tx-$RUN 25.00)"
call "bet" 200 -X POST $API/wagering/transactions -H "Authorization: Bearer $PA" -H "Idempotency-Key: provider-a:tx-$RUN" -d "$(bet provider-a tx-$RUN 25.00)"
TX=$(field transactionId)
call "bet replay" 200 -X POST $API/wagering/transactions -H "Authorization: Bearer $PA" -H "Idempotency-Key: provider-a:tx-$RUN" -d "$(bet provider-a tx-$RUN 25.00)"
call "same key other payload" 409 -X POST $API/wagering/transactions -H "Authorization: Bearer $PA" -H "Idempotency-Key: provider-a:tx-$RUN" -d "$(bet provider-a tx-$RUN 26.00)"
call "same external id other key" 409 -X POST $API/wagering/transactions -H "Authorization: Bearer $PA" -H "Idempotency-Key: other-$RUN" -d "$(bet provider-a tx-$RUN 25.00)"
call "bet without funds" 422 -X POST $API/wagering/transactions -H "Authorization: Bearer $PA" -H "Idempotency-Key: provider-a:big-$RUN" -d "$(bet provider-a big-$RUN 5000.00)"
call "refund before reference" 202 -X POST $API/wagering/transactions -H "Authorization: Bearer $PA" -H "Idempotency-Key: provider-a:refund-$RUN" -d "$(bet provider-a refund-$RUN 10.00 REFUND missing-$RUN)"
call "opening via http" 400 -X POST $API/wagering/transactions -H "Authorization: Bearer $PA" -H "Idempotency-Key: provider-a:open-$RUN" -d "$(bet provider-a open-$RUN 10.00 OPENING)"

call "get transaction as owner" 200 $API/wagering/transactions/$TX -H "Authorization: Bearer $PA"
call "get transaction as other provider" 404 $API/wagering/transactions/$TX -H "Authorization: Bearer $PB"
call "get transaction as internal" 200 $API/wagering/transactions/$TX -H "Authorization: Bearer $INTERNAL"
call "get by external id as owner" 200 $API/providers/provider-a/wagering/transactions/tx-$RUN -H "Authorization: Bearer $PA"
call "get by external id as other provider" 403 $API/providers/provider-a/wagering/transactions/tx-$RUN -H "Authorization: Bearer $PB"
call "get wallet as provider" 403 $API/wallets/$WALLET -H "Authorization: Bearer $PA"
call "get wallet" 200 $API/wallets/$WALLET -H "Authorization: Bearer $INTERNAL"
call "ledger" 200 "$API/wallets/$WALLET/ledger?limit=1" -H "Authorization: Bearer $INTERNAL"
call "reconciliation" 200 -X POST $API/wallets/$WALLET/reconciliation -H "Authorization: Bearer $INTERNAL"

SHORT=$(token provider-short-lived)
sleep 4
call "expired token" 401 $API/wagering/transactions/$TX -H "Authorization: Bearer $SHORT"
