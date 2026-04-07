package marumasa.crimebuster

import org.bukkit.plugin.java.JavaPlugin

class CrimeBuster : JavaPlugin() {

    lateinit var dataManager: DataManager
    lateinit var aiAnalyzer: AIAnalyzer
    lateinit var punishmentManager: PunishmentManager
    lateinit var chatHistoryManager: ChatHistoryManager

    override fun onEnable() {
        saveDefaultConfig()

        dataManager = DataManager(this)
        aiAnalyzer = AIAnalyzer(this)
        aiAnalyzer.loadPrompt()
        punishmentManager = PunishmentManager(this, dataManager)
        
        val maxHistory = config.getInt("analysis.chat_history_size", 20)
        chatHistoryManager = ChatHistoryManager(maxHistory)

        // サーバーが既に稼働中で、リロードされた際のためにオンラインプレイヤーをロード
        server.onlinePlayers.forEach { dataManager.loadPlayerState(it.uniqueId) }

        // イベント登録
        server.pluginManager.registerEvents(
            EventListeners(this, dataManager, aiAnalyzer, punishmentManager, chatHistoryManager),
            this
        )

        // コマンド登録
        val crimeCommand = CrimeCommand(this, dataManager, aiAnalyzer)
        getCommand("crime")?.let {
            it.setExecutor(crimeCommand)
            it.tabCompleter = crimeCommand
        }

        logger.info("CrimeBuster enabled.")
        
        startDecayTask()
        startAutoSaveTask()
    }

    /**
     * 犯罪係数を時間経過で減少させるタスク
     */
    private fun startDecayTask() {
        val intervalMinutes = config.getLong("decay.interval_minutes", 60)
        val amount = config.getInt("decay.amount", 1)
        val ticks = intervalMinutes * 60 * 20

        server.scheduler.runTaskTimer(this, Runnable {
            server.onlinePlayers.forEach { player ->
                val state = dataManager.updateCrime(player.uniqueId, -amount, "Time Decay")
                if (amount > 0 && state.crime >= 0) {
                    // ログ出力の条件を微修正
                }
            }
        }, ticks, ticks)
    }

    /**
     * キャッシュされたデータを定期的にディスクに保存するタスク
     */
    private fun startAutoSaveTask() {
        val intervalMinutes = config.getLong("persistence.auto_save_minutes", 5)
        val intervalTicks = intervalMinutes * 60 * 20L
        server.scheduler.runTaskTimerAsynchronously(this, Runnable {
            logger.info("Auto-saving all player data...")
            dataManager.saveAll()
        }, intervalTicks, intervalTicks)
    }

    override fun onDisable() {
        // シャットダウン時にすべてのデータを確実に対比
        logger.info("Saving all player data before disable...")
        dataManager.saveAll()
        logger.info("CrimeBuster disabled.")
    }
}
