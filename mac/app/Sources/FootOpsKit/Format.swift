import Foundation

/// Numbers, sizes and times in Turkish, as the core writes them for the Terminal
/// (internal/mac/print.go): a decimal comma, "1,5 GB", "15:04", "3 dk önce".
public enum Fmt {
    /// v with the given decimals and a decimal comma.
    public static func number(_ v: Double, decimals: Int) -> String {
        String(format: "%.\(decimals)f", v).replacingOccurrences(of: ".", with: ",")
    }

    /// A size in GB (one decimal) or MB.
    public static func size(_ n: UInt64) -> String {
        if n >= 1 << 30 {
            return number(Double(n) / Double(1 << 30), decimals: 1) + " GB"
        }
        return number(Double(n) / Double(1 << 20), decimals: 0) + " MB"
    }

    public static func percent(_ used: UInt64, of total: UInt64) -> Int {
        total == 0 ? 0 : Int((100 * Double(used) / Double(total)).rounded())
    }

    /// "665 ms" under a second, "2,8 sn" above.
    public static func duration(ms: Int) -> String {
        ms < 1000 ? "\(ms) ms" : number(Double(ms) / 1000, decimals: 1) + " sn"
    }

    /// A span in Turkish: "3 dk", "2 sa 5 dk", "3 gün 4 sa" (at least a minute).
    public static func span(_ seconds: TimeInterval) -> String {
        let m = max(Int(seconds / 60), 1)
        switch m {
        case ..<60:
            return "\(m) dk"
        case ..<(24 * 60):
            return m % 60 == 0 ? "\(m / 60) sa" : "\(m / 60) sa \(m % 60) dk"
        default:
            let h = m / 60
            return h % 24 == 0 ? "\(h / 24) gün" : "\(h / 24) gün \(h % 24) sa"
        }
    }

    /// "az önce" within a minute, else "3 dk önce".
    public static func ago(_ date: Date, now: Date) -> String {
        now.timeIntervalSince(date) < 60 ? "az önce" : span(now.timeIntervalSince(date)) + " önce"
    }

    /// "15:04" for today, "02.01 15:04" otherwise, in the zone given (the Mac's).
    public static func clock(_ date: Date, now: Date, zone: TimeZone = .current) -> String {
        var cal = Calendar(identifier: .gregorian)
        cal.timeZone = zone
        let f = DateFormatter()
        f.locale = Locale(identifier: "tr_TR")
        f.timeZone = zone
        f.dateFormat = cal.isDate(date, inSameDayAs: now) ? "HH:mm" : "dd.MM HH:mm"
        return f.string(from: date)
    }

    /// The first letter in capitals, the Turkish way (i is İ).
    public static func upperFirst(_ s: String) -> String {
        guard let first = s.first else { return s }
        return String(first).uppercased(with: Locale(identifier: "tr_TR")) + s.dropFirst()
    }
}
