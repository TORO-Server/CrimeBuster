package marumasa.crimebuster

import io.papermc.paper.event.player.AsyncChatEvent
import net.kyori.adventure.text.serializer.plain.PlainComponentSerializer
import org.bukkit.Bukkit
import org.bukkit.entity.Player
import org.bukkit.event.EventHandler
import org.bukkit.event.Listener
import org.bukkit.event.player.PlayerJoinEvent
import org.bukkit.event.player.PlayerQuitEvent
import java.util.UUID
import java.util.concurrent.ConcurrentHashMap
import java.util.concurrent.atomic.AtomicInteger
import java.util.concurrent.atomic.AtomicLong

class EventListeners(
    private val plugin: CrimeBuster,
    private val dataManager: DataManager,
    private val aiAnalyzer: AIAnalyzer,
    private val punishmentManager: PunishmentManager,
    private val chatHistoryManager: ChatHistoryManager
) : Listener {

    @EventHandler
    fun onJoin(event: PlayerJoinEvent) {
        // プレイヤーのデータを準備
        dataManager.loadPlayerState(event.player.uniqueId)
    }

    @EventHandler
    fun onQuit(event: PlayerQuitEvent) {
        val uuid = event.player.uniqueId
        // データを保存してキャッシュから削除
        dataManager.unloadPlayerState(uuid)
        
        // メモリリーク防止：クールダウンデータおよび履歴ハッシュの削除
        analysisCooldowns.remove(uuid)
        lastMessages.remove(uuid)
        // 親密度関連のクールダウン（対象プレイヤーが関与する全ペア）を削除
        intimacyCooldowns.keys.removeIf { it.first == uuid || it.second == uuid }
    }

    // クールダウン管理 (UUID -> 最終実行ミリ秒)
    private val analysisCooldowns = ConcurrentHashMap<UUID, Long>()
    private val intimacyCooldowns = ConcurrentHashMap<Pair<UUID, UUID>, Long>()
    
    // グローバルレートリミット用のカウンター（DoS 対策）
    private val globalRequestCount = AtomicInteger(0)
    private val lastGlobalReset = AtomicLong(System.currentTimeMillis())
    
    // 直前のメッセージ履歴 (UUID -> String) のハッシュ
    private val lastMessages = ConcurrentHashMap<UUID, Int>()

    // プレイヤーごとのバースト制限（1秒あたりの最大メッセージ数）
    private val burstCounter = ConcurrentHashMap<UUID, AtomicInteger>()
    private val lastBurstReset = ConcurrentHashMap<UUID, AtomicLong>()
    
    // 現在実行中のAIリクエスト数（メモリおよびAPI負荷管理）
    private val pendingRequests = AtomicInteger(0)

    @EventHandler
    fun onChat(event: AsyncChatEvent) {
        val player = event.player
        val now = System.currentTimeMillis()
        
        // AI解析のクールダウンチェック
        val analysisCooldownMs = plugin.config.getLong("cooldown.chat_analysis", 5).coerceAtLeast(1) * 1000
        val lastAnalysis = analysisCooldowns.getOrDefault(player.uniqueId, 0L)
        
        val message = PlainComponentSerializer.plain().serialize(event.message())
        
        // メッセージ長制限のチェック（設定値から取得、デフォルト 1000 文字）
        val maxLength = plugin.config.getInt("analysis.max_message_length", 1000).coerceAtLeast(10)
        if (message.length > maxLength) {
            return
        }
        
        // 1. バースト制限チェック（連投対策）
        val playerBurstLimit = plugin.config.getInt("analysis.player_burst_limit", 3).coerceAtLeast(1)
        val lastResetTime = lastBurstReset.getOrPut(player.uniqueId) { AtomicLong(now) }.get()
        val counter = burstCounter.getOrPut(player.uniqueId) { AtomicInteger(0) }
        
        if (now - lastResetTime > 1000) {
            counter.set(0)
            lastBurstReset[player.uniqueId]?.set(now)
        }
        if (counter.incrementAndGet() > playerBurstLimit) {
            return
        }

        // 2. グローバルレートリミットチェック（DoS 対策）
        val globalLimit = plugin.config.getInt("gemma.global_rate_limit", 10).coerceAtLeast(1)
        val lastReset = lastGlobalReset.get()
        if (now - lastReset > 1000) {
            if (lastGlobalReset.compareAndSet(lastReset, now)) {
                globalRequestCount.set(0)
            }
        }
        if (globalRequestCount.incrementAndGet() > globalLimit) {
            return
        }

        // 3. キューサイズ制限（メモリ保護）
        val maxPending = plugin.config.getInt("gemma.max_pending_requests", 50).coerceAtLeast(5)
        if (pendingRequests.get() >= maxPending) {
            plugin.logger.warning("AI Analysis dropped for ${player.name}: Global pending queue full.")
            return
        }

        // 短時間の同一メッセージの連続解析を防止
        val msgHash = message.hashCode()
        if (lastMessages[player.uniqueId] == msgHash && now - lastAnalysis < 30000) {
            return
        }

        // 全体のチャット履歴の更新は常に行う
        chatHistoryManager.addMessage(player.name, message)

        if (now - lastAnalysis < analysisCooldownMs) {
            // クールダウン中のため解析をスキップ
            return
        }
        analysisCooldowns[player.uniqueId] = now
        lastMessages[player.uniqueId] = msgHash

        val playerState = dataManager.getPlayerState(player.uniqueId)
        val historyStr = chatHistoryManager.getHistoryAsString()
        val intimacyStr = playerState.intimacy.toString()
        val playerHistory = playerState.history

        // AI 解析を非同期で実行
        pendingRequests.incrementAndGet()
        aiAnalyzer.analyzeChat(player.name, message, historyStr, intimacyStr, playerHistory).thenAccept { increment ->
            pendingRequests.decrementAndGet()
            // 解析結果をメインスレッドに反映
            // 解析結果をメインスレッドに反映
            Bukkit.getScheduler().runTask(plugin, Runnable {
                // 犯罪係数の更新
                val state = dataManager.updateCrime(player.uniqueId, increment, "AI Analysis")
                
                // 親密度の更新（メインスレッドで実行）
                val intimacyCooldownMs = plugin.config.getLong("cooldown.intimacy_gain", 30).coerceAtLeast(1) * 1000
                val maxIntimacy = plugin.config.getInt("analysis.max_intimacy_level", 100).coerceAtLeast(1)
                
                player.getNearbyEntities(20.0, 20.0, 20.0).filterIsInstance<Player>().forEach { nearbyPlayer ->
                    if (nearbyPlayer.uniqueId != player.uniqueId) {
                        val pair = player.uniqueId to nearbyPlayer.uniqueId
                        val lastIntimacy = intimacyCooldowns.getOrDefault(pair, 0L)
                        
                        if (now - lastIntimacy >= intimacyCooldownMs) {
                            val targetUuidStr = nearbyPlayer.uniqueId.toString()
                            val current = state.intimacy.getOrDefault(targetUuidStr, 0)
                            if (current < maxIntimacy) {
                                state.intimacy[targetUuidStr] = current + 1
                                intimacyCooldowns[pair] = now
                            }
                        }
                    }
                }

                if (increment != 0) {
                    val direction = if (increment > 0) "committed a potential crime" else "showed exemplary behavior"
                    plugin.logger.info("Player ${player.name}: CrimeScore updated to ${state.crime} ($increment).")
                    punishmentManager.checkAndPunish(player, state)
                }
            })
        }
    }
}
