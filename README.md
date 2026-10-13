# Processamento distribuído de apostas em Go

Serviço em Go, composto com Uber Fx, que movimenta carteiras de jogadores a partir de operações de provedores de jogos recebidas por HTTP e por SQS. O enunciado está em [DESAFIO.md](DESAFIO.md) e as decisões técnicas em [ARCHITECTURE.md](ARCHITECTURE.md).

## Pré-requisitos

- Docker com Docker Compose v2.
- Go 1.27.2, apenas para rodar a aplicação ou os testes fora do Compose.
- `curl` e `bash`, para os exemplos e para `scripts/smoke.sh`.

## Subir tudo

```sh
docker compose up --build
```

Isso sobe, nesta ordem:

| Serviço | Endereço no host | Papel |
| --- | --- | --- |
| `postgres` | `localhost:5432` | Banco |
| `migrate` | — | Aplica as migrations e encerra |
| `keycloak` | `localhost:8081` | IdP; importa o realm `wagering` |
| `localstack` | `localhost:4566` | SQS |
| `sqs-init` | — | Cria as filas e encerra |
| `app` (3 réplicas) | `localhost:8090`, `8091`, `8092` | A aplicação |

Cada réplica é um processo independente, com suas conexões, seu consumidor SQS, seu publisher da outbox e seu worker de referências.

Para verificar o fluxo autenticado de ponta a ponta (29 chamadas, com o status esperado de cada uma):

```sh
bash scripts/smoke.sh
```

## Variáveis de ambiente

[.env.example](.env.example) traz os valores para rodar a aplicação fora do Compose.

| Variável | Padrão | Descrição |
| --- | --- | --- |
| `HTTP_ADDR` | `:8080` | Endereço de escuta |
| `HTTP_REQUEST_TIMEOUT` | `10s` | Prazo de cada requisição |
| `SHUTDOWN_TIMEOUT` | `20s` | Prazo total do encerramento |
| `DATABASE_URL` | obrigatória | DSN do PostgreSQL |
| `OIDC_ISSUER` | obrigatória | Valor exigido na claim `iss` |
| `OIDC_JWKS_URL` | obrigatória | Onde ler as chaves públicas do IdP |
| `OIDC_AUDIENCE` | obrigatória | Valor exigido na claim `aud` |
| `AWS_REGION`, `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY` | obrigatórias | Lidas pelo SDK da AWS |
| `AWS_ENDPOINT_URL` | — | Endpoint do LocalStack |
| `SQS_INPUT_QUEUE` | `wager-transactions.fifo` | Fila de operações |
| `SQS_DEAD_LETTER_QUEUE` | `wager-transactions-dlq.fifo` | DLQ |
| `SQS_EVENTS_QUEUE` | `wager-events.fifo` | Destino dos eventos |
| `SQS_VISIBILITY_TIMEOUT` | `30s` | Invisibilidade da mensagem em tratamento |
| `SQS_CONCURRENCY` | `8` | Mensagens tratadas em paralelo |
| `OUTBOX_LEASE` | `30s` | Prazo para retomar um evento reservado |
| `REFERENCE_MAX_ATTEMPTS` | `6` | Tentativas do worker antes de rejeitar |
| `REFERENCE_BASE_BACKOFF` / `REFERENCE_MAX_BACKOFF` | `2s` / `1m` | Backoff exponencial das referências |
| `REFERENCE_POLL_INTERVAL` | `1s` | Frequência do worker de referências |

A aplicação não sobe com configuração inválida: todos os problemas são listados de uma vez.

## Migrations

Ficam em [migrations/](migrations/) e são aplicadas com [golang-migrate](https://github.com/golang-migrate/migrate).

```sh
docker compose run --rm migrate            # aplica (o `up` do Compose já faz isso)
docker compose run --rm migrate down 1     # reverte a última
docker compose run --rm migrate version    # versão atual
```

Para recomeçar do zero, apagando o volume do banco local: `docker compose down -v`.

## Filas

O serviço `sqs-init` cria as três filas FIFO a partir de [deploy/sqs/](deploy/sqs/):

| Fila | Uso |
| --- | --- |
| `wager-transactions.fifo` | Entrada; redrive para a DLQ após 5 recebimentos |
| `wager-transactions-dlq.fifo` | Mensagens inválidas, em conflito ou com tentativas esgotadas |
| `wager-events.fifo` | Eventos de integração publicados pela outbox |

Para recriá-las: `docker compose run --rm sqs-init`.

## Rodar a aplicação fora do Compose

```sh
docker compose up -d postgres migrate keycloak localstack sqs-init
set -a && . ./.env.example && set +a
go run ./cmd/server
```

Nesse modo a aplicação escuta em `localhost:8080`; use `API=http://localhost:8080 bash scripts/smoke.sh`.

## Autenticação

O Keycloak sobe com o realm `wagering` já provisionado ([deploy/keycloak/wagering-realm.json](deploy/keycloak/wagering-realm.json)). As identidades de teste usam `client_credentials`; o segredo de cada uma é `<client_id>-secret`.

| `client_id` | Permissão |
| --- | --- |
| `provider-a`, `provider-b` | Provedor: envia e consulta as próprias transações |
| `wallet-service` | Serviço interno: carteiras, ledger, reconciliação, consulta de qualquer transação |
| `provider-short-lived` | Provedor com token de 2 segundos, para testar expiração |
| `other-audience` | Sem a audiência da API: seus tokens são recusados |

Console de administração: <http://localhost:8081> (`admin` / `admin`).

Obter um token:

```sh
token() {
  curl -s http://localhost:8081/realms/wagering/protocol/openid-connect/token \
    -d grant_type=client_credentials -d client_id="$1" -d client_secret="$1-secret" |
    sed -E 's/.*"access_token":"([^"]+)".*/\1/'
}
INTERNAL=$(token wallet-service)
PROVIDER=$(token provider-a)
```

## Exemplos de chamadas

```sh
API=http://localhost:8090
PLAYER=0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1

# Abrir carteira (serviço interno)
curl -s -X POST $API/wallets -H "Authorization: Bearer $INTERNAL" \
  -d "{\"playerId\":\"$PLAYER\",\"initialBalance\":{\"amount\":\"1000.00\",\"currency\":\"BRL\"}}"
WALLET=<id devolvido>

# Aposta (provedor)
curl -s -X POST $API/wagering/transactions \
  -H "Authorization: Bearer $PROVIDER" -H "Idempotency-Key: provider-a:transaction-123" \
  -d "{\"providerId\":\"provider-a\",\"externalTransactionId\":\"transaction-123\",\"playerId\":\"$PLAYER\",\"walletId\":\"$WALLET\",\"roundId\":\"round-987\",\"gameId\":\"fortune-chimp\",\"kind\":\"BET\",\"money\":{\"amount\":\"25.00\",\"currency\":\"BRL\"}}"

# Consultas
curl -s $API/providers/provider-a/wagering/transactions/transaction-123 -H "Authorization: Bearer $PROVIDER"
curl -s $API/wallets/$WALLET -H "Authorization: Bearer $INTERNAL"
curl -s "$API/wallets/$WALLET/ledger?limit=50" -H "Authorization: Bearer $INTERNAL"
curl -s -X POST $API/wallets/$WALLET/reconciliation -H "Authorization: Bearer $INTERNAL"

# Públicos
curl -s $API/health/live
curl -s $API/health/ready
curl -s $API/metrics
```

Enviar a mesma aposta pela fila (o `MessageGroupId` é a carteira e o `MessageDeduplicationId` é o `messageId`):

```sh
docker compose run --rm --entrypoint aws sqs-init sqs send-message \
  --queue-url http://localstack:4566/000000000000/wager-transactions.fifo \
  --message-group-id "$WALLET" --message-deduplication-id msg-123 \
  --message-body "{\"messageId\":\"msg-123\",\"type\":\"WagerTransactionRequested\",\"occurredAt\":\"2026-09-08T12:00:00.000Z\",\"data\":{\"providerId\":\"provider-a\",\"externalTransactionId\":\"transaction-123\",\"idempotencyKey\":\"provider-a:transaction-123\",\"playerId\":\"$PLAYER\",\"walletId\":\"$WALLET\",\"roundId\":\"round-987\",\"gameId\":\"fortune-chimp\",\"kind\":\"BET\",\"money\":{\"amount\":\"25.00\",\"currency\":\"BRL\"}}}"
```

Os códigos HTTP e os corpos de cada situação estão em [ARCHITECTURE.md](ARCHITECTURE.md#contrato-http).

## Testes

Há três níveis, separados por build tags.

### Unitários

Não dependem de infraestrutura.

```sh
go vet ./...
go test ./...
```

### Integração (`integration`)

Usam o PostgreSQL, o Keycloak e o LocalStack reais do Compose. Não há mocks de infraestrutura.

```sh
docker compose up -d postgres migrate keycloak localstack sqs-init
go test -p 1 -count=1 -tags integration ./...
```

`-p 1` roda os pacotes em série: eles compartilham o banco, e o publisher da outbox reserva qualquer evento pendente. O ledger é imutável até para `TRUNCATE`, então os testes usam identificadores novos em vez de limpar tabelas.

| Pacote | O que verifica |
| --- | --- |
| `internal/postgres` | Constraints, imutabilidade do ledger, locks, outbox concorrente, inbox |
| `internal/wagering` | Regras dos cinco tipos, idempotência, reversões, referências pendentes, concorrência entre instâncias |
| `internal/messaging` | Consumidor, reentrega, retry, DLQ, shutdown, publishers concorrentes e recuperação |
| `internal/app` | Composição Fx: início, parada, liberação de recursos, falha rápida |

### Ponta a ponta, múltiplas instâncias e falhas (`e2e`)

Compilam o binário e sobem **três processos independentes** do sistema operacional, cada um com sua memória e suas conexões, e os derrubam de forma abrupta em alguns cenários.

```sh
docker compose up -d postgres migrate keycloak localstack sqs-init
docker compose stop app     # para as réplicas do Compose não disputarem a fila
go test -count=1 -tags e2e ./test/e2e/
```

| Teste | Cenário |
| --- | --- |
| `TestAuthenticationAndProviderIsolation` | IdP real: token ausente, inválido, adulterado, de outra audiência e expirado; isolamento entre provedores |
| `TestSameBetFiftyTimesAcrossProcesses` | Mesma aposta 50 vezes em paralelo: um débito |
| `TestTwoBetsRaceAcrossProcesses` | Duas apostas de 80.00 sobre 100.00 |
| `TestSameOperationThroughHTTPAndSQS` | Mesma operação pelos dois canais ao mesmo tempo |
| `TestRestartPreservesIdempotencyAndPendingReferences` | Queda de todos os processos; a pendência é retomada pela nova instância |
| `TestAbruptKillLosesNothing` | Dois de três processos mortos durante o consumo: nada se perde nem duplica, todo evento é publicado |

### Detector de corrida

`go test -race` exige cgo. Onde houver um compilador C:

```sh
go test -race ./...
go test -race -p 1 -count=1 -tags integration ./...
```

Sem compilador C (por exemplo, no Windows), rode dentro de um contêiner na rede do Compose:

```sh
docker run --rm --network backend-challenge-go_default -v "$PWD:/src" -w /src \
  -e TEST_DATABASE_URL="postgres://jungle:devpassword@postgres:5432/jungle_challenge?sslmode=disable" \
  -e DATABASE_URL="postgres://jungle:devpassword@postgres:5432/jungle_challenge?sslmode=disable" \
  -e OIDC_JWKS_URL="http://keycloak:8080/realms/wagering/protocol/openid-connect/certs" \
  -e AWS_ENDPOINT_URL="http://localstack:4566" -e AWS_REGION=us-east-1 \
  -e AWS_ACCESS_KEY_ID=test -e AWS_SECRET_ACCESS_KEY=test \
  golang:1.27.2 sh -c "go test -race ./... && go test -race -p 1 -count=1 -tags integration ./..."
```

O nome da rede segue o do diretório do projeto; confira com `docker network ls`.

### Simular falhas manualmente

```sh
docker compose kill app                      # queda abrupta das três réplicas
docker compose up -d app                     # restart: pendências e outbox são retomadas
docker compose stop localstack               # SQS fora: /health/ready responde 503 e os eventos ficam na outbox
docker compose start localstack && docker compose run --rm sqs-init
docker compose stop postgres                 # banco fora: a API responde 503 com Retry-After
```

## Estrutura

```
cmd/server            ponto de entrada
internal/domain       Money, Wallet, WagerTransaction, ledger, eventos e hash (sem dependências externas)
internal/postgres     transações SQL e repositórios (pgx)
internal/wagering     casos de uso, compartilhados por HTTP e SQS
internal/httpapi      rotas, autorização e contrato HTTP
internal/auth         validação de tokens OIDC
internal/messaging    consumidor SQS e publisher da outbox
internal/metrics      métricas Prometheus
internal/config       configuração por ambiente
internal/app          composição com Uber Fx e ciclo de vida
migrations            schema versionado
deploy                realm do Keycloak e atributos das filas
test/e2e              testes com processos reais
```
