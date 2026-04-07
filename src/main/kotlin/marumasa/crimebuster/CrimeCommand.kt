package marumasa.crimebuster

import net.kyori.adventure.text.Component
import net.kyori.adventure.text.format.NamedTextColor
import org.bukkit.Bukkit
import org.bukkit.command.Command
import org.bukkit.command.CommandExecutor
import org.bukkit.command.CommandSender
import org.bukkit.command.TabCompleter
import org.bukkit.entity.Player

class CrimeCommand(
    private val plugin: CrimeBuster,
    private val dataManager: DataManager,
    private val aiAnalyzer: AIAnalyzer
) : CommandExecutor, TabCompleter {

    override fun onCommand(sender: CommandSender, command: Command, label: String, args: Array<out String>): Boolean {
        if (args.isEmpty()) {
            if (sender !is Player) {
                sender.sendMessage("このコマンドはプレイヤーのみ実行可能です。")
                return true
            }
            val state = dataManager.getPlayerState(sender.uniqueId)
            sender.sendMessage(Component.text("あなたの現在の犯罪係数: ", NamedTextColor.GRAY)
                .append(Component.text(state.crime, NamedTextColor.YELLOW)))
            return true
        }

        when (args[0].lowercase()) {
            "info" -> {
                if (!sender.hasPermission("crimebuster.admin")) {
                    sender.sendMessage(Component.text("権限がありません。", NamedTextColor.RED))
                    return true
                }
                if (args.size < 2) return false
                val target = Bukkit.getOfflinePlayer(args[1])
                if (!target.hasPlayedBefore() && !target.isOnline) {
                    sender.sendMessage(Component.text("対象プレイヤーが見つかりません。", NamedTextColor.RED))
                    return true
                }
                val state = dataManager.getPlayerState(target.uniqueId)
                sender.sendMessage(Component.text("${target.name ?: args[1]} の犯罪係数: ", NamedTextColor.GRAY)
                    .append(Component.text(state.crime, NamedTextColor.YELLOW)))
            }
            "set" -> {
                if (!sender.hasPermission("crimebuster.admin")) {
                    sender.sendMessage(Component.text("権限がありません。", NamedTextColor.RED))
                    return true
                }
                if (args.size < 3) return false
                val target = Bukkit.getOfflinePlayer(args[1])
                if (!target.hasPlayedBefore() && !target.isOnline) {
                    sender.sendMessage(Component.text("対象プレイヤーが見つかりません。", NamedTextColor.RED))
                    return true
                }
                val value = args[2].toIntOrNull() ?: return false
                val delta = value - dataManager.getPlayerState(target.uniqueId).crime
                dataManager.updateCrime(target.uniqueId, delta, "Admin Set")
                sender.sendMessage(Component.text("${target.name ?: args[1]} の犯罪係数を $value に設定しました。", NamedTextColor.GREEN))
            }
            "reset" -> {
                if (!sender.hasPermission("crimebuster.admin")) {
                    sender.sendMessage(Component.text("権限がありません。", NamedTextColor.RED))
                    return true
                }
                if (args.size < 2) return false
                val target = Bukkit.getOfflinePlayer(args[1])
                if (!target.hasPlayedBefore() && !target.isOnline) {
                    sender.sendMessage(Component.text("対象プレイヤーが見つかりません。", NamedTextColor.RED))
                    return true
                }
                val state = dataManager.getPlayerState(target.uniqueId)
                dataManager.updateCrime(target.uniqueId, -state.crime, "Admin Reset")
                sender.sendMessage(Component.text("${target.name ?: args[1]} の犯罪係数をリセットしました。", NamedTextColor.GREEN))
            }
            "reload" -> {
                if (!sender.hasPermission("crimebuster.admin")) {
                    sender.sendMessage(Component.text("権限がありません。", NamedTextColor.RED))
                    return true
                }
                plugin.reloadConfig()
                aiAnalyzer.loadPrompt()
                sender.sendMessage(Component.text("設定とプロンプトをリロードしました。", NamedTextColor.GREEN))
            }
            else -> return false
        }

        return true
    }

    override fun onTabComplete(sender: CommandSender, command: Command, alias: String, args: Array<out String>): List<String> {
        if (!sender.hasPermission("crimebuster.admin")) return emptyList()
        
        return when (args.size) {
            1 -> listOf("info", "set", "reset", "reload").filter { it.startsWith(args[0].lowercase()) }
            2 -> Bukkit.getOnlinePlayers().map { it.name }.filter { it.startsWith(args[1], ignoreCase = true) }
            else -> emptyList()
        }
    }
}
