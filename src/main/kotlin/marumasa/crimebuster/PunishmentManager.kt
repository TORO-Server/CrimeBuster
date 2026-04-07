package marumasa.crimebuster

import net.kyori.adventure.text.Component
import net.kyori.adventure.text.format.NamedTextColor
import org.bukkit.Bukkit
import org.bukkit.entity.Player
import java.util.*

class PunishmentManager(
    private val plugin: CrimeBuster,
    private val dataManager: DataManager
) {
    private val kickThreshold: Int
        get() = plugin.config.getInt("thresholds.kick", 70)
    private val banThreshold: Int
        get() = plugin.config.getInt("thresholds.ban", 100)

    fun checkAndPunish(player: Player, state: PlayerState) {
        val crime = state.crime

        when {
            crime >= banThreshold -> executeBan(player)
            crime >= kickThreshold -> executeKick(player)
        }
    }

    private fun executeKick(player: Player) {
        // メインスレッドで実行する必要があるため Scheduler を使用
        Bukkit.getScheduler().runTask(plugin, Runnable {
            player.kick(Component.text("犯罪係数が高すぎます。一旦頭を冷やしてください。", NamedTextColor.RED))
        })
    }

    private fun executeBan(player: Player) {
        val durationDays = plugin.config.getLong("durations.ban", 1)
        val reason = "犯罪係数が限界（$banThreshold）に達しました。"
        
        // 1日 = 24 * 60 * 60 * 1000 ms
        val expiration = Date(System.currentTimeMillis() + durationDays * 24 * 60 * 60 * 1000)

        Bukkit.getScheduler().runTask(plugin, Runnable {
            // UUIDベースでの追放。プレイヤーが名前を変更しても回避できないようにする。
            try {
                // 型パラメータを明示的に指定
                val profileBanList = Bukkit.getBanList<org.bukkit.BanList<org.bukkit.profile.PlayerProfile>>(org.bukkit.BanList.Type.PROFILE)
                profileBanList.addBan(player.playerProfile, reason, expiration, "CrimeBuster")
            } catch (e: Throwable) {
                // 古いサーバー環境向け、またはエラー時のフォールバック（名前ベース）
                val nameBanList = Bukkit.getBanList<org.bukkit.BanList<String>>(org.bukkit.BanList.Type.NAME)
                nameBanList.addBan(player.name, reason, expiration, "CrimeBuster")
            }
            
            player.kick(Component.text("犯罪係数が限界に達したため、追放されました（期間: $durationDays 日）。\n理由: $reason", NamedTextColor.RED))
            plugin.logger.info("${player.name} (${player.uniqueId}) has been banned for $durationDays days due to high crime coefficient.")
        })
    }
}
