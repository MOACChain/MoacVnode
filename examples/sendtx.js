/*
 * Example programs to test under MOAC console
 *  
 */
var src=mc.accounts[0];
var des=mc.accounts[1];

/*
 * Display some numbers
*/
function checkB() {
    var totalBal = 0;
    // for (var acctNum in mc.accounts) {
    for (var acctNum =0; acctNum < 10; acctNum ++) {
        var acct = mc.accounts[acctNum];
        var acctBal = chain3.fromSha(mc.getBalance(acct), "mc");
        totalBal += parseFloat(acctBal);
        console.log("  mc.accounts[" + acctNum + "]: \t" + acct + " \tbalance: " + acctBal + " mc");
    }

};
var cache = [
  '',
  ' ',
  '  ',
  '   ',
  '    ',
  '     ',
  '      ',
  '       ',
  '        ',
  '         '
];

function leftPad (str, len, ch) {
    // convert `str` to `string`
    str = str + '';
    // `len` is the `pad`'s length now
    len = len - str.length;
    // doesn't need to pad
    if (len <= 0) return str;
    // `ch` defaults to `' '`
    if (!ch && ch !== 0) ch = ' ';
    // convert `ch` to `string`
    ch = ch + '';
    // cache common use cases
    if (ch === ' ' && len < 10) return cache[len] + str;
    // `pad` starts with an empty string
    var pad = '';
    // loop
    while (true) {
        // add `ch` to `pad` if `len` is odd
        if (len & 1) pad += ch;
        // divide `len` by 2, ditch the remainder
        len >>= 1;
        // "double" the `ch` so this operation count grows logarithmically on `len`
        // each time `ch` is "doubled", the `len` would need to be "doubled" too
        // similar to finding a value in binary search tree, hence O(log(n))
        if (len) ch += ch;
        // `len` is 0, exit the loop
        else break;
    }
    // pad `str`!
    return pad + str;
}

function sendtx(src, tgtaddr, amount, strData) {

    //var amt = leftPad(chain3.toHex(chain3.toSha(amount)).slice(2).toString(16),64,0);
    //var strData = '';
        
    chain3.mc.sendTransaction(
        {
            from: src,
            // nonce: 205,
            value:chain3.toSha(amount,'mc'),
            to: tgtaddr,
            gas: "1200",
            gasPrice: "30000000000",//chain3.mc.gasPrice,
            data: strData//,
            // shardingFlag:0,
            // via : '0x0000000000000000000000000000000000000000'
        }, function (e, transactionHash){
            if (!e) {
                 console.log('Transaction hash: ' + transactionHash);
            }else{
                console.log('Error:'+e);
            }
         });
        
    console.log('sending from:' +   src + ' to:' + tgtaddr  + ' amount:' + amount + ' with data:' + strData);

}

function Send(src, passwd, target, value, indata)
{
    chain3.personal.unlockAccount(src, passwd, 0);
    sendtx(src, target, value, indata );
    
}

function FutureSend(src, passwd, target, value, block)
{

    var str = "000000000000000000000000AAAA";   
    var strtgt = str.replace("AAAA", target.substring(2));
        
    var amt = leftPad(chain3.toHex(chain3.toSha(value, 'mc')).slice(2).toString(16),64,0);

    var blkstr = leftPad(chain3.toHex(block).slice(2).toString(16),64,0);

    var strData = "0xdef0412f";
    strData = strData + blkstr + strtgt + amt;

    chain3.personal.unlockAccount(src, passwd, 0);
    var src = src;
    var cntaddr = "0x0000000000000000000000000000000000000065";
    sendtx(src, cntaddr, '0', strData );
    
}


scs1="";
scs2="";
scs3="";

var registerdata = "0x4420e486000000000000000000000000ECd1e094Ee13d0B47b72F5c940C17bD0c7630326";
function registeropen(subAddress) {
    sendtx(chain3.mc.coinbase, subAddress, 0, "0x5defc56c");
}

function addfundtosubchain(src, subAddress, value) {
    sendtx(src, subAddress, value, "0xa2f09dfa");
}

function registerclose() {
    sendtx(chain3.mc.coinbase, subAddress, 0, "0x69f3576f");
}

function registertopool(contractadd, scsaddress) {
    var registerdata = "0x4420e486000000000000000000000000"+scsaddress.substring(2);
    sendtx(chain3.mc.coinbase, contractadd, 12, registerdata);
}

