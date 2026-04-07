package marumasa.crimebuster

import com.google.gson.Gson
import com.google.gson.GsonBuilder
import java.io.File
import java.util.*
import java.util.concurrent.ConcurrentHashMap

class DataManager(private val plugin: CrimeBuster) {
    private val gson: Gson = GsonBuilder().setPrettyPrinting().create()
    private val folder = File(plugin.dataFolder, "players")
    
    // プレイヤーデータをメモリ上に保持するキャッシュ
    private val cache = ConcurrentHashMap<UUID, PlayerState>()

    init {
        if (!folder.exists()) {
            folder.mkdirs()
        }
    }

    private fun getFile(uuid: UUID): File {
        val uuidStr = uuid.toString()
        // 防衛的プログラミング：UUID形式を確認
        if (!uuidStr.matches(Regex("^[0-9a-fA-F-]{36}$"))) {
            throw IllegalArgumentException("Invalid UUID: $uuidStr")
        }
        return File(folder, "$uuidStr.json")
    }

    /**
     * ログイン時などにファイルからデータをロードしてキャッシュに格納する。
     */
    fun loadPlayerState(uuid: UUID) {
        val file = getFile(uuid)
        val state = if (file.exists()) {
            try {
                file.reader().use { gson.fromJson(it, PlayerState::class.java) } ?: PlayerState()
            } catch (e: Exception) {
                plugin.logger.warning("Failed to load player data for $uuid: ${e.message}")
                PlayerState()
            }
        } else {
            PlayerState()
        }
        cache[uuid] = state
    }

    /**
     * 指定したプレイヤーのデータを取得する。キャッシュにない場合はロードを試みる。
     */
    fun getPlayerState(uuid: UUID): PlayerState {
        return cache.computeIfAbsent(uuid) {
            val file = getFile(uuid)
            if (file.exists()) {
                file.reader().use { gson.fromJson(it, PlayerState::class.java) } ?: PlayerState()
            } else {
                PlayerState()
            }
        }
    }

    /**
     * 指定したプレイヤーのキャッシュデータをファイルに保存する。
     */
    fun savePlayerState(uuid: UUID) {
        synchronized(this) {
            val state = cache[uuid] ?: return
            val file = getFile(uuid)
            val tmpFile = File(folder, "${uuid}.json.tmp")
            try {
                tmpFile.writer().use { gson.toJson(state, it) }
                // アトミックな移動を試行。OSレベルで保護する。
                java.nio.file.Files.move(
                    tmpFile.toPath(),
                    file.toPath(),
                    java.nio.file.StandardCopyOption.REPLACE_EXISTING,
                    java.nio.file.StandardCopyOption.ATOMIC_MOVE
                )
            } catch (e: Exception) {
                plugin.logger.severe("Failed to save player data for $uuid: ${e.message}")
                if (tmpFile.exists()) tmpFile.delete()
            }
        }
    }


    /**
     * キャッシュされている全プレイヤーのデータをファイルに保存する。
     */
    fun saveAll() {
        cache.keys.forEach { savePlayerState(it) }
    }

    /**
     * キャッシュからデータを削除し、保存する（ログアウト時など）。
     */
    fun unloadPlayerState(uuid: UUID) {
        savePlayerState(uuid)
        cache.remove(uuid)
    }

    /**
     * 犯罪係数を更新する。特定条件（閾値超え等）で即時保存を行う。
     */
    fun updateCrime(uuid: UUID, delta: Int, reason: String? = null): PlayerState = synchronized(this) {
        val state = getPlayerState(uuid)
        val oldCrime = state.crime
        state.crime = maxOf(0, state.crime + delta)

        // 履歴の更新（最新10件を保持）
        if (reason != null && delta != 0) {
            val limitedReason = if (reason.length > 100) reason.substring(0, 97) + "..." else reason
            val historyList = if (state.history.isEmpty()) mutableListOf() else state.history.split("\n").toMutableList()
            historyList.add(0, "[${java.time.Instant.now()}] $limitedReason (Change: $delta, New: ${state.crime})")
            state.history = historyList.take(10).joinToString("\n")
        }
        
        // 親密度データの肥大化防止（最大 50 プレイヤー分のみ保持）
        if (state.intimacy.size > 50) {
            // スコアが低い順に削除（あるいはランダムに削除）
            val sortedKeys = state.intimacy.entries.sortedBy { it.value }.map { it.key }
            for (i in 0 until (state.intimacy.size - 50)) {
                state.intimacy.remove(sortedKeys[i])
            }
        }
        
        // 設定値以上の犯罪係数になった場合、または大きな変動があった場合は即時保存
        val threshold = plugin.config.getInt("persistence.immediate_save_threshold", 50)
        if (state.crime >= threshold || (state.crime - oldCrime).let { it >= 10 || it <= -10 }) {
            savePlayerState(uuid)
        }
        
        return state
    }
}

data class PlayerState(
    var crime: Int = 0,
    val intimacy: ConcurrentHashMap<String, Int> = ConcurrentHashMap(),
    var history: String = ""
)
