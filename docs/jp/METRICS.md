### **Open Match 2 テレメトリー**

Open Match 2はOpenTelemetryを使用して、コアの操作、RPCコール、キャッシュのパフォーマンスに関する詳細なメトリクスを出力し、マッチメイキングプロセスに対する深い可視性を提供します 。

このドキュメントでは、om-coreがインポートするgolangモジュールによって標準で提供される一部のメトリクス（rpcなど）については触れません 。

Open Match 2に対して記述するマッチメーカーからもOpenTelemetryを使用してテレメトリーを出力することを推奨します。マッチメーカーのテレメトリーの例については、[open-match-ecosystemリポジトリのサンプル](https://github.com/googleforgames/open-match-ecosystem/blob/main/v2/examples/METRICS-JP.md)を参照してください 。

#### **Open Match 2でOpenTelemetryを有効化および設定する方法**

Open Match 2でOpenTelemetryを有効にするには、`OM_OTEL_SIDECAR`環境変数を`true`に設定する必要があります 。

デフォルトでは、Open MatchはローカルのOpenTelemetryサイドカーにメトリクスをエクスポートしようとします 。

サイドカーなしで実行している場合（例えば、ローカル開発時）は、`OM_OTEL_SIDECAR`を`false`に設定できます 。

適切な環境変数を設定することで、OpenTelemetryエクスポーターを設定できます 。

詳細については、OpenTelemetryのドキュメントを参照してください 。

#### **コアメトリクス**

これらのメトリクスは`metrics.go`で定義されており、Open Matchサービスの全体的な健全性とパフォーマンスに関する洞察を提供します 。

| メトリクス名 | 説明 | 単位 |
| ----- | ----- | ----- |
| `om_core.ticket.size` | 正常に作成されたチケットのサイズ。 | kb |
| `om_core.rpc.duration` | RPCの実行時間。 | ms |
| `om_core.rpc.errors` | 返されたgRPCエラーの総数。 | count |
| `om_core.ticket.activation.requests` | アクティブに設定され、プールに表示されるようになったチケットの総数。 | count |
| `om_core.ticket.activation.failures.invalid_id` | `ActivateTickets` RPCコールごとに、無効なIDが原因で失敗したチケットアクティベーションの数。 | count |
| `om_core.ticket.deactivation.failures.unspecified` | `ActivateTickets` RPCコールごとに、不特定のエラーが原因で失敗したチケットアクティベーションの数。 | count |
| `om_core.ticket.deactivation.requests` | `DeactivateTickets` RPCコールごとに、非アクティブに設定されたチケットの数。 | count |
| `om_core.ticket.deactivation.failures.invalid_id` | `DeactivateTickets` RPCコールごとに、無効なIDが原因で失敗したチケット非アクティブ化の数。 | count |
| `om_core.ticket.deactivation.failures.unspecified` | `DeactivateTickets` RPCコールごとに、不特定のエラーが原因で失敗したチケット非アクティブ化の数。 | count |
| `om_core.ticket.assignment` | 各`CreateAssignments()`呼び出しに含まれるチケットアサインメントの数。 | count |
| `om_core.ticket.assigment.watch` | 各`CreateAssignments()`呼び出しに含まれるチケットアサインメントの数。 | count |
| `om_core.profile.chunks` | `InvokeMatchmakingFunctions()`呼び出し中に、すべてのプールにすべてのチケットが投入された後、プロファイルが分割されたチャンクの数。 | count |
| `om_core.cache.tickets.available` | `InvokeMatchmakingFunctions()`呼び出しが提供されたプロファイルのプールフィルターを処理した時点で、キャッシュ内にあったアクティブなチケットの数。 | count |
| `om_core.profile.pools` | `InvokeMatchmakingFunctions()`呼び出しに提供されたプロファイル内のプールの数。 | count |
| `om_core.mmf.failures` | MMF（マッチメイキング機能）の失敗回数。 | count |
| `om_core.mmf.deactivations` | MMFによってマッチで返されたチケットによる非アクティブ化の数。 | count |
| `om_core.match.received` | MMFから受信したマッチの総数。 | count |

#### **キャッシュメトリクス**

これらのメトリクスは`internal/statestore/cache/metrics.go`で定義されており、レプリケートされたチケットキャッシュのパフォーマンスに関する洞察を提供します 。

| メトリクス名 | 説明 | 単位 |
| ----- | ----- | ----- |
| `om_core.cache.tickets.active` | プールに表示される可能性のあるアクティブなチケット。 | count |
| `om_core.cache.tickets.inactive` | プールに表示されない非アクティブなチケット。 | count |
| `om_core.cache.assignments` | Open Matchキャッシュ内のアサインメント。 | count |
| `om_core.cache.expiration.duration` | キャッシュの有効期限切れ処理ロジックの実行時間。 | ms |
| `om_core.cache.assignment.expirations` | キャッシュの有効期限切れサイクルごとに期限切れになったアサインメントの数。 | count |
| `om_core.cache.outgoing.updates` | サイクルごとの送信レプリケーション更新数。 | count |
| `om_core.cache.ticket.expirations` | キャッシュの有効期限切れサイクルごとに期限切れになったチケットの数。 | count |
| `om_core.cache.incoming.updates` | ポーリングごとの受信レプリケーション更新数。 | count |
| `om_core.cache.ticket.inactive.expirations` | キャッシュの有効期限切れサイクルごとに期限切れになった非アクティブなチケットの数。 | count |
| `om_core.cache.outgoing.timeouts` | 送信レプリケーションキューが`OM_CACHE_OUT_WAIT_TIMEOUT_MS`待機しても、バッチとしてレプリケーターに送信するための`OM_CACHE_OUT_MAX_QUEUE_THRESHOLD`の更新数に達しなかった回数。 | count |
| `om_core.cache.outgoing.maxqueuethresholdreached` | 送信レプリケーションキューが`OM_CACHE_OUT_WAIT_TIMEOUT_MS`ミリ秒未満で`OM_CACHE_OUT_MAX_QUEUE_THRESHOLD`の更新数に達した回数。 | count |
| `om_core.cache.incoming.timeouts.empty` | 受信レプリケーションキューが`OM_CACHE_IN_WAIT_TIMEOUT_MS`待機した後に更新がなかった回数。 | count |
| `om_core.cache.incoming.timeouts.full` | 受信レプリケーションキューが500ミリ秒以内に保留中のすべての更新を処理できなかった回数。  |  |

