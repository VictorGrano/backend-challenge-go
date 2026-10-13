# Arquitetura e decisões

Este documento registra as decisões da solução, as interpretações adotadas onde o enunciado deixava margem e o que não foi feito. As instruções de execução estão no [README](README.md).

## Visão geral

```
HTTP ──┐                                 ┌── wallets
       ├─► wagering.Service ─► uma ──────┼── wager_transactions
SQS ───┘   (mesmo caso de uso) transação ├── wallet_ledger_entries
                               SQL       ├── inbox_messages
                                         └── outbox_events ─► publisher ─► wager-events.fifo
```

HTTP e SQS chamam o mesmo método, `Service.Submit`. Tudo o que uma operação altera é confirmado em um único commit. Os eventos saem depois, por um worker que lê a outbox.

O pacote `internal/domain` só importa a biblioteca padrão: não conhece Fx, HTTP, SQS nem pgx.

## Dinheiro

- **Representação:** `Money` é um value object com `int64` em unidades mínimas (centavos) e a moeda ISO 4217. Não há `float` em nenhum ponto: parsing, cálculo, JSON e persistência trabalham com inteiros e strings.
- **Limites:** ±92.233.720.368.547.758,07. Overflow é detectado no parsing, na soma, na subtração e na negação, e devolve `ErrMoneyOverflow`.
- **Escala:** fixa em duas casas. As moedas aceitas são uma lista de moedas com duas casas decimais (BRL, USD, EUR e outras); moedas com outra escala, como JPY, ficam de fora.
- **Entrada:** `ParseMoney` aceita `"25"`, `"25.5"` e `"25.50"`. Rejeita vazio, espaços, sinal, `"1."`, `".5"`, mais de duas casas, notação científica, `NaN` e `Infinity`. Nada é arredondado. Um valor JSON numérico (`25.00` sem aspas) é recusado na decodificação.
- **Normalização antes do hash:** as formas equivalentes viram `"25.50"`; zeros à esquerda são aceitos e removidos.
- **Negativos:** permitidos em cálculos e na `difference` da reconciliação; recusados nas entradas externas e no saldo da carteira.
- **Persistência:** `BIGINT` em centavos mais `CHAR(3)` para a moeda.
- **Valor zero de `Money`** (não inicializado) não tem moeda e é rejeitado por todas as operações.

## Modelo de domínio

Entidades têm campos privados, construtor com validação, função de reidratação separada e métodos de transição. A reidratação valida a consistência do estado, mas não reaplica movimentações nem emite eventos. Erros são sentinelas comparáveis com `errors.Is`; não há `panic` para regras de negócio.

`updatedAt` nunca retrocede: se uma transição for feita por uma instância com o relógio atrasado, o valor anterior é mantido.

### Máquina de estados da transação

```
PENDING ──► PROCESSED
   │   ├──► REJECTED
   │   └──► FAILED
   └──► PENDING_REFERENCE ──► PROCESSED | REJECTED | FAILED
```

`PROCESSED`, `REJECTED` e `FAILED` são terminais: o domínio recusa novas transições e um trigger no banco recusa o `UPDATE`.

**Processamento síncrono.** Uma operação sem dependências é criada, processada e confirmada no mesmo commit; não existe um `PENDING` confirmado no banco. O único estado intermediário durável é `PENDING_REFERENCE`, retomado pelo worker de referências.

**Falhas transitórias e permanentes.** Uma falha transitória (banco indisponível, prazo estourado, deadlock) desfaz a transação SQL e não deixa registro: o cliente recebe 503 e repete com a mesma chave, ou o SQS reentrega a mensagem. `postgres.IsTransient` faz essa classificação. O estado `FAILED` está modelado no domínio e no schema, mas **nenhum caminho do código o produz hoje**: não identifiquei, neste escopo, uma falha de infraestrutura que fosse permanente para uma operação já aceita. Mensagens que não podem ser tratadas vão para a DLQ.

## Transações SQL

- **Biblioteca:** `pgx` v5 com SQL explícito, sem ORM.
- **Delimitação:** `DB.WithinTx` abre a transação e a coloca no `context.Context`. Cada repositório usa a transação do contexto, ou o pool se não houver. Assim, repositórios diferentes participam do mesmo commit sem se conhecerem.
- **Isolamento:** `READ COMMITTED`, com lock de linha explícito. A reconciliação usa `WithinSnapshot` (`REPEATABLE READ`, somente leitura) para ler saldo e ledger na mesma visão.

## Concorrência

**Estratégia:** lock pessimista por carteira, com `SELECT ... FOR UPDATE` na linha da carteira no início de cada operação. Operações da mesma carteira são serializadas; carteiras diferentes não se bloqueiam. Não há lock global nem lock em memória, então a garantia vale entre processos.

Escolhi o lock pessimista em vez do controle otimista porque a disputa esperada é sobre a mesma carteira (várias operações de uma rodada), e nesse padrão o otimista gera repetições, enquanto o pessimista apenas enfileira.

**Segunda linha de defesa, no banco:**

- `UPDATE wallets ... WHERE version = <versão lida>`: se a linha mudou, nada é gravado e o repositório devolve `ErrConcurrentUpdate`.
- `CHECK (balance >= 0)` na carteira.
- `CHECK (balance_after = balance_before ± amount)` no ledger.

## Idempotência

A idempotência está no banco e sobrevive ao reinício de todos os processos.

| Garantia | Mecanismo |
| --- | --- |
| Uma operação por provedor | `UNIQUE (provider_id, external_transaction_id)` |
| Uma chave por provedor | `UNIQUE (provider_id, idempotency_key)` |
| Conflito de conteúdo | `payload_hash` comparado no replay |
| Uma mensagem por consumidor | `PRIMARY KEY (consumer_name, message_id)` na inbox |

**Hash do payload:** SHA-256, em hexadecimal, do JSON canônico (chaves em ordem alfabética, sem espaços) com os campos `providerId`, `externalTransactionId`, `playerId`, `walletId`, `roundId`, `gameId`, `kind`, `money.amount`, `money.currency` e, quando informado, `referenceExternalTransactionId`. A chave de idempotência e os metadados de transporte (`messageId`, `occurredAt`, headers) ficam de fora, o que torna o hash igual para HTTP e SQS.

**Normalizações antes do hash:** valor monetário com duas casas; `playerId` e `walletId` na forma canônica de UUID (minúsculas). Os demais textos entram exatamente como recebidos.

**Comportamento:**

- Mesma chave e mesmo conteúdo: devolve o resultado persistido, com `idempotentReplay: true`. Para uma operação processada, o saldo devolvido é o observado no processamento original (`result_balance`), não o saldo atual.
- Mesma chave e conteúdo diferente: conflito.
- Mesmo `(providerId, externalTransactionId)` com **outra chave**: conflito. A operação não é reaplicada e também não é tratada como replay. É uma interpretação: o enunciado só exige que não seja reaplicada.
- A chave é escopada por provedor, para que um provedor não colida com as chaves de outro nem as sonde.

**Corrida entre recebimentos idênticos.** A busca pela operação existente é feita duas vezes: antes do lock (caminho rápido) e de novo com a carteira travada, quando já enxerga o commit de outra instância. As duas unicidades são consultadas em **uma única query**: duas consultas separadas poderiam ver, entre uma e outra, um commit alheio e reportar um conflito falso (um teste de concorrência pegou exatamente isso). Se ainda houver colisão na inserção, a transação SQL é refeita, até três vezes.

## Operações e reversões

| Tipo | Movimento | Observações |
| --- | --- | --- |
| `BET` | Débito | Saldo insuficiente rejeita com `INSUFFICIENT_FUNDS` |
| `WIN` | Crédito | Referência opcional a uma `BET` |
| `LOSS` | Nenhum | Valor `0.00`; sem ledger, sem mudança de versão; emite só `WagerTransactionProcessed` |
| `REFUND` | Crédito | Referência obrigatória a uma `BET` processada |
| `ROLLBACK` | Contrário ao original | Crédito se a referência é `BET`; débito se é `WIN` ou `REFUND` |

A operação e a referência precisam concordar em provedor, jogador, carteira, moeda e rodada, e o tipo referenciado precisa ser compatível. Em `REFUND` e `ROLLBACK` o valor tem de ser idêntico: não há reversão parcial.

**Combinações de `REFUND` e `ROLLBACK`.** Uma transação aceita **no máximo uma reversão bem-sucedida, de qualquer um dos dois tipos**. Isso é garantido por um índice único parcial sobre `reference_transaction_id` e verificado antes pelo caso de uso, com a carteira travada. Consequências:

- `REFUND` seguido de `ROLLBACK` da mesma aposta (ou o inverso): o segundo é rejeitado com `REFERENCE_ALREADY_REVERSED`. O débito não é devolvido duas vezes.
- O próprio `REFUND` pode ser desfeito por um `ROLLBACK` que o referencie (débito).
- Limitação assumida: depois que um `REFUND` sofre `ROLLBACK`, a aposta original não pode ser reembolsada de novo, porque o primeiro `REFUND` continua registrado como processado.

Uma reversão que precisaria debitar mais que o saldo é rejeitada com `REVERSAL_INSUFFICIENT_FUNDS`, código diferente do de aposta sem saldo, e fica registrada.

### Referências ainda indisponíveis

Se a referência não existe, a operação é gravada como `PENDING_REFERENCE` com a data da próxima tentativa, o evento `WagerTransactionPendingReference` é emitido e a resposta é 202. O worker de cada instância reserva as pendências vencidas com `FOR UPDATE SKIP LOCKED`, de modo que várias instâncias não tratam a mesma. Como o estado e a agenda estão no banco, a retomada independe de quem registrou a pendência.

- **Backoff:** exponencial, de `REFERENCE_BASE_BACKOFF` até `REFERENCE_MAX_BACKOFF`.
- **Limite:** `REFERENCE_MAX_ATTEMPTS` tentativas do worker. Esgotadas, a operação vira `REJECTED` com `REFERENCE_NOT_FOUND` e o evento de rejeição.
- **Referência existe, mas está pendente:** a operação continua aguardando; se as tentativas se esgotarem, é rejeitada com `REFERENCE_NOT_PROCESSED`.
- **Referência terminou em `REJECTED` ou `FAILED`:** rejeição imediata com `REFERENCE_NOT_PROCESSED`, pois não há o que reverter.

### Códigos de falha

| `failureCode` | Significado | Natureza |
| --- | --- | --- |
| `INSUFFICIENT_FUNDS` | Aposta sem saldo | Definitiva |
| `REVERSAL_INSUFFICIENT_FUNDS` | Reversão debitaria mais que o saldo | Definitiva |
| `REFERENCE_NOT_FOUND` | Referência não chegou no prazo | Definitiva |
| `REFERENCE_NOT_PROCESSED` | Referência não foi concluída com sucesso | Definitiva |
| `REFERENCE_MISMATCH` | Referência diverge em jogador, carteira, moeda, rodada, tipo ou valor | Definitiva |
| `REFERENCE_ALREADY_REVERSED` | Referência já tem uma reversão | Definitiva |
| `WALLET_MISMATCH` | A carteira não pertence ao jogador informado | Definitiva |
| `CURRENCY_MISMATCH` | Moeda diferente da carteira | Definitiva |

Toda rejeição com `failureCode` é definitiva e fica gravada: repetir a mesma chave devolve o mesmo resultado. As **entradas corrigíveis** não geram transação nem `failureCode`: voltam como erro 400, 404 ou 409 (tabela abaixo) e o cliente pode corrigir e reenviar.

## Contrato HTTP

`POST /wagering/transactions` devolve sempre o mesmo corpo de resultado quando a operação existe:

| Situação | Status | Corpo |
| --- | --- | --- |
| Processada (primeira vez ou replay) | 200 | `status: PROCESSED`, `balance`, `idempotentReplay` |
| Aguardando referência | 202 | `status: PENDING_REFERENCE` |
| Rejeição de negócio | 422 | `status: REJECTED`, `failureCode` |

Erros usam `{"error":{"code","message","correlationId"}}`:

| Situação | Status | `code` |
| --- | --- | --- |
| Entrada inválida, `Idempotency-Key` ausente, tipo `OPENING` | 400 | `INVALID_INPUT` |
| Token ausente, inválido ou expirado | 401 | `UNAUTHENTICATED` |
| Sem permissão, ou `providerId` de outro provedor | 403 | `FORBIDDEN` |
| Carteira inexistente | 404 | `WALLET_NOT_FOUND` |
| Transação inexistente ou de outro provedor | 404 | `TRANSACTION_NOT_FOUND` |
| Jogador já tem carteira na moeda | 409 | `WALLET_ALREADY_EXISTS` |
| Chave reutilizada com outro conteúdo | 409 | `IDEMPOTENCY_KEY_CONFLICT` |
| ID externo já enviado com outra chave | 409 | `EXTERNAL_TRANSACTION_CONFLICT` |
| Indisponibilidade transitória | 503 | `TEMPORARILY_UNAVAILABLE`, com `Retry-After` |
| Erro inesperado | 500 | `INTERNAL_ERROR` |

Uma operação para uma carteira inexistente não é persistida (a transação tem chave estrangeira para a carteira): é tratada como entrada corrigível.

`playerId`, `walletId` e `transactionId` precisam ser UUID, como nos exemplos do enunciado.

O ledger é paginado em ordem de gravação por um cursor opaco (a posição do último lançamento, codificada), com `limit` de 1 a 200 e padrão 50.

## Ledger

Append-only. Um trigger recusa `UPDATE`, `DELETE` e `TRUNCATE`. Além disso:

- `UNIQUE (wallet_id, transaction_id)`.
- Chave estrangeira composta para a carteira com a moeda, e para a transação com a carteira: um lançamento não pode apontar uma transação de outra carteira nem usar outra moeda.
- `CHECK` da invariante de saldos.

A abertura com saldo positivo grava uma transação `OPENING` (única por carteira, por índice parcial), o lançamento de crédito de zero ao saldo inicial e os dois eventos, com a carteira na versão 1. Saldo zero cria só a carteira. O ledger é de partida simples.

## Inbox, outbox e SQS

### Consumidor

- **Identidade da mensagem:** o `messageId` do envelope, por consumidor. A inbox guarda o hash do corpo; um `messageId` repetido com outro corpo vai para a DLQ.
- **Atomicidade:** o registro na inbox, a operação, o ledger, os eventos e a conclusão da inbox ficam na mesma transação SQL. Se algo falhar, o registro da inbox também é desfeito e a reentrega recomeça do zero.
- **Remoção:** só depois do commit. Se o processo morrer entre o commit e a remoção, o SQS reentrega e a inbox reconhece.
- **Referência pendente:** a mensagem é concluída assim que a pendência está gravada; o worker de referências assume.
- **Rejeição de negócio:** é um resultado confirmado e terminal; a mensagem é removida.

| Situação | Destino |
| --- | --- |
| JSON inválido, envelope incompleto, tipo desconhecido, valor não textual | DLQ, enviada pela aplicação com o atributo `failureReason` |
| Entrada inválida para o domínio (por exemplo `OPENING`) | DLQ |
| Conflito de idempotência ou de `messageId` | DLQ |
| Falha transitória ou carteira inexistente | Nova tentativa com backoff |
| Tentativas esgotadas | DLQ, pela redrive policy do SQS |

- **Limites:** `maxReceiveCount` 5 na redrive policy. Visibility timeout de 30 s, com o tratamento limitado à metade. Após falha transitória, a mensagem volta a ficar visível em 2 s × 2^(recebimentos−1), até 60 s.
- **`MessageGroupId`:** o `walletId`, definido por quem publica. Mantém a ordem por carteira e permite paralelismo entre carteiras. A correção não depende disso: o lock da carteira e o estado `PENDING_REFERENCE` tratam mensagens fora de ordem.
- **`MessageDeduplicationId`:** o `messageId`. A deduplicação do FIFO é só uma otimização; os testes de duplicidade enviam cada cópia com um ID de deduplicação diferente, para que todas cheguem à aplicação.
- **Autorização:** o acesso às filas é controlado pelas credenciais e políticas do broker. O consumidor confia no `providerId` da mensagem e aplica as mesmas validações de domínio do HTTP. O LocalStack não impõe políticas IAM; em produção, cada produtor deveria ter permissão de envio restrita.

### Outbox e publisher

Os eventos são gravados em `outbox_events` na transação que os originou, então nada é publicado antes do commit.

- **Disputa:** cada publisher reserva um lote com `FOR UPDATE SKIP LOCKED`, e a reserva empurra `next_attempt_at` para `agora + lease`.
- **Trabalho abandonado:** se o publisher morrer, o evento volta a ser elegível quando o lease vence e outra instância o assume.
- **Falha de envio:** grava `last_error` e reagenda com backoff exponencial.
- **Entrega at-least-once:** uma queda entre o envio e a confirmação provoca republicação com o mesmo `eventId`.
- **Snapshot:** o payload fica em uma coluna `JSON` (preserva os bytes), e um trigger impede alterar identidade, tipo ou conteúdo do evento.

### Contrato dos eventos

Destino: `wager-events.fifo`.

| Campo SQS | Valor |
| --- | --- |
| `MessageGroupId` | `aggregateId` (a transação, ou a carteira em `WalletBalanceChanged`) |
| `MessageDeduplicationId` | `eventId` |
| Atributos | `eventType`, `eventId` |
| Corpo | Envelope JSON |

Envelope: `eventId`, `eventType`, `aggregateId`, `correlationId`, `causationId` (opcional), `occurredAt` (RFC 3339, UTC), `version` (1) e `data`. Valores monetários são strings decimais. O `correlationId` vem do header `X-Correlation-ID` ou do `messageId`; o `causationId` é o `messageId` quando a origem é SQS.

**O que o consumidor dos eventos deve assumir:**

- Pode receber o mesmo evento mais de uma vez: deduplique por `eventId`. O FIFO só descarta repetições dentro de cinco minutos.
- A ordem dentro de um agregado não é garantida quando há falha de envio, porque um evento em backoff pode ser ultrapassado pelo seguinte. Para a carteira, use `walletVersion` para ordenar.

## Autenticação e autorização

- **IdP:** Keycloak, provisionado por importação de realm. A aplicação não emite tokens nem guarda senhas.
- **Fluxo:** `client_credentials`, por ser comunicação entre serviços.
- **Validação:** assinatura pelas chaves do JWKS do IdP, `iss`, `aud` (`wagering-api`) e validade, com `go-oidc`. As chaves são buscadas sob demanda e renovadas na rotação.
- **Issuer e JWKS separados:** no Compose, quem pede o token usa `localhost:8081` e a aplicação alcança o Keycloak por `keycloak:8080`. O Keycloak é configurado com hostname fixo, então a claim `iss` é sempre a mesma.

**Modelo de permissões:**

| Role | Quem | Pode |
| --- | --- | --- |
| `provider` | Clientes dos provedores | Enviar operações e consultar as próprias transações |
| `internal` | `wallet-service` | Abrir e ler carteiras, ledger, reconciliação, consultar qualquer transação |

O provedor autorizado vem da claim `provider_id`, configurada no IdP por cliente. O `providerId` do corpo precisa ser igual a ela; caso contrário a resposta é 403 e o caso de uso não é chamado. Isso vale também para replays: um provedor que repete a chave de outro recebe 403, sem o resultado.

Na consulta por ID interno, uma transação de outro provedor responde 404, para não revelar que o identificador existe. Na consulta por `providerId` no caminho, a resposta é 403.

`/health/live`, `/health/ready` e `/metrics` são públicos. As métricas não contêm dados de negócio, mas em produção esse endpoint ficaria restrito à rede interna.

## Uber Fx e ciclo de vida

A composição está em `internal/app`, em módulos (`fx.Module`): configuração, observabilidade, banco, casos de uso, autenticação, mensageria, HTTP e workers. Tudo é injetado por construtor.

**Início** (`OnStart`, na ordem de dependência):

1. Configuração validada na construção do grafo.
2. `Ping` no PostgreSQL.
3. Resolução das URLs das três filas (falha se o SQS estiver fora ou faltar uma fila).
4. Abertura da porta HTTP.
5. Início dos workers.

Qualquer falha impede a subida.

**Parada** (`OnStop`, na ordem inversa), dentro de `SHUTDOWN_TIMEOUT`:

1. O worker de referências e o publisher têm o contexto cancelado. O publisher devolve à fila da outbox os eventos reservados e não enviados.
2. O consumidor para de buscar mensagens e conclui as que estão em andamento. Se isso não couber em metade do prazo, os tratamentos são cancelados e as mensagens liberadas (visibilidade zero) para reentrega.
3. O servidor HTTP para de aceitar conexões e aguarda as requisições em andamento.
4. O pool do banco é fechado.

Cada hook só retorna quando a goroutine correspondente terminou, ou devolve erro se o prazo estourar. Um teste sobe a aplicação inteira, para e verifica que a porta foi liberada e que não sobrou goroutine da aplicação.

## Observabilidade

- **Logs:** JSON (`slog`), com `correlationId`, `messageId`, `transactionId`, `walletId` e `providerId` quando disponíveis. Tokens, corpos e valores financeiros não são registrados; a exceção é a diferença apontada por uma reconciliação divergente.
- **Métricas** (`/metrics`, Prometheus):

| Métrica | Conteúdo |
| --- | --- |
| `wager_transactions_total{status,kind,source}` | Resultados por estado |
| `wager_duplicates_total{source}` | Recebimentos repetidos reconhecidos |
| `wager_processing_duration_seconds{source}` | Latência de processamento |
| `wager_concurrency_conflicts_total{kind}` | Colisões de inserção e conflitos de idempotência |
| `sqs_messages_total{outcome}`, `sqs_retries_total`, `sqs_dlq_total{reason}` | Consumo, retries e DLQ |
| `outbox_publish_total{result}`, `outbox_lag_seconds` | Publicação e atraso da outbox |
| `wager_reference_retries_total` | Reprocessamentos de referência |
| `wallet_reconciliation_divergences_total` | Divergências de reconciliação |

- **Health checks:** liveness do processo; readiness de PostgreSQL e SQS.

`sqs_dlq_total` conta só as mensagens que a aplicação envia à DLQ. As movidas pela redrive policy do SQS não passam pela aplicação.

A reconciliação reporta a divergência na resposta, em um log de erro e na métrica, e não altera o saldo.

## Limitações e trabalho não concluído

- **`FAILED` não é produzido** por nenhum caminho do código (ver "Falhas transitórias e permanentes").
- **Sem `PENDING` confirmado:** o processamento é síncrono, então o cenário "interromper após confirmar `PENDING`" não se aplica; o equivalente coberto é a retomada de `PENDING_REFERENCE` por outra instância.
- **Uma reversão por referência**, com a consequência descrita em "Operações e reversões".
- **Moedas:** apenas as de duas casas da lista.
- **Ordem dos eventos** não é garantida sob falha de envio.
- **Autorização no SQS** depende das políticas do broker, que o LocalStack não impõe.
- **Os casos de uso dependem dos repositórios concretos** de `internal/postgres`, sem interfaces intermediárias. Por isso as regras de orquestração são testadas contra o PostgreSQL real, e não há testes unitários delas com dublês.
- **Testes de integração compartilham o banco** e precisam de `-p 1`.
- **Métricas das referências pendentes e do atraso da outbox** são por instância; não há painel agregado.
- **Não feitos (opcionais):** ledger de partidas dobradas, tracing com OpenTelemetry, dashboards e testes de carga.
