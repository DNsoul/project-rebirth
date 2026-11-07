/// Where each region the servers are placed in is, in the user's words.
const regionNames = {
  'sto': 'Стокгольм',
  'waw': 'Варшава',
  'fra': 'Франкфурт',
  'ams': 'Амстердам',
};

String regionLabel(String code) => regionNames[code] ?? code;
