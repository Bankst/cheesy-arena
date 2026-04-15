// Client-side logic for the Quick Play page. Reuses the websocket and handlers from
// match_play.js; only the Load & Arm flow and localStorage hydration live here.

const quickPlayStorageKey = "quickPlayTeams";

const readQuickPlayTeams = function () {
  return {
    Red1: parseInt($("#qpRed1").val()) || 0,
    Red2: parseInt($("#qpRed2").val()) || 0,
    Red3: parseInt($("#qpRed3").val()) || 0,
    Blue1: parseInt($("#qpBlue1").val()) || 0,
    Blue2: parseInt($("#qpBlue2").val()) || 0,
    Blue3: parseInt($("#qpBlue3").val()) || 0,
  };
};

const writeQuickPlayTeams = function (teams) {
  $("#qpRed1").val(teams.Red1 || "");
  $("#qpRed2").val(teams.Red2 || "");
  $("#qpRed3").val(teams.Red3 || "");
  $("#qpBlue1").val(teams.Blue1 || "");
  $("#qpBlue2").val(teams.Blue2 || "");
  $("#qpBlue3").val(teams.Blue3 || "");
};

// Sends a websocket message to load a test match with the entered team numbers.
const quickPlayMatch = function () {
  const teams = readQuickPlayTeams();
  try {
    localStorage.setItem(quickPlayStorageKey, JSON.stringify(teams));
  } catch (e) {
    // Ignore - localStorage may be unavailable (private mode, quota, etc.).
  }
  websocket.send("quickPlayMatch", teams);
};

$(function () {
  try {
    const stored = localStorage.getItem(quickPlayStorageKey);
    if (stored) {
      writeQuickPlayTeams(JSON.parse(stored));
    }
  } catch (e) {
    // Ignore.
  }
});
